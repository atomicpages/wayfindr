//go:build !windows

package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/daemonlock"
)

// The whole background lifecycle runs the real binary: `start` must start
// a separate `wayfindr start --foreground` that outlives it, which only a
// separate executable can show. The go build cache keeps the build quick.
func TestServeDetachLogsAndStopRunTheDaemonInTheBackground(t *testing.T) {
	goTool, err := exec.LookPath("go")

	if err != nil {
		t.Skip("go tool not on PATH")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "wayfindr")
	build := exec.CommandContext(t.Context(), goTool, "build", "-o", bin, "github.com/atomicpages/wayfindr/cmd/wayfindr") //nolint:gosec // G204: the go tool building this module

	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	listen := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	config := filepath.Join(dir, "config", "wayfindr.toml")
	dataDir := filepath.Join(dir, "data")
	writeFile(t, config, fmt.Sprintf("[server]\nlisten = %q\n", listen))
	key := make([]byte, 32)
	token := make([]byte, 32)

	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	if _, err := rand.Read(token); err != nil {
		t.Fatal(err)
	}

	env := []string{
		"HOME=" + dir,
		"PATH=" + os.Getenv("PATH"),
		"WAYFINDR_CONFIG=" + config,
		"WAYFINDR_DATA_DIR=" + dataDir,
		"WAYFINDR_SECRET_KEY=" + base64.StdEncoding.EncodeToString(key),
		"WAYFINDR_CLIENT_TOKEN=wf_" + base64.RawURLEncoding.EncodeToString(token),
	}

	run := func(args ...string) (string, error) {
		t.Helper()
		// Not t.Context(): stop also runs at cleanup.
		cmd := exec.CommandContext(context.WithoutCancel(t.Context()), bin, args...) //nolint:gosec // G204: the binary this test built
		cmd.Env = env
		out, err := cmd.CombinedOutput()

		return string(out), err
	}

	// Whatever happens, no daemon outlives the test.
	t.Cleanup(func() { _, _ = run("stop") })

	out, err := run("start")

	if err != nil {
		t.Fatalf("start: %v\n%s", err, out)
	}

	m := regexp.MustCompile(`^Daemon running at http://` + regexp.QuoteMeta(listen) + ` \(pid (\d+)\)\nLog: ` + regexp.QuoteMeta(filepath.Join(dataDir, "wayfindr.log")) + "\n$").FindStringSubmatch(out)

	if m == nil {
		t.Fatalf("start printed %q, want the address, pid, and log", out)
	}

	pid := m[1]
	holder, held, err := daemonlock.Probe(filepath.Join(dataDir, "daemon.lock"))

	if err != nil || !held || fmt.Sprint(holder.PID) != pid {
		t.Fatalf("after start: lock holder %+v, held %v, err %v; want pid %s", holder, held, err, pid)
	}

	if out, err := run("start"); err != nil || out != "Daemon already running at http://"+listen+" (pid "+pid+")\n" {
		t.Errorf("start again: %q, %v; want it to report the running daemon", out, err)
	}

	if out, err := run("logs", "-n", "1"); err != nil || strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "{") {
		t.Errorf("logs -n 1: %q, %v; want one JSON line", out, err)
	}

	if out, err := run("stop"); err != nil || out != "Stopped the daemon (pid "+pid+")\n" {
		t.Fatalf("stop: %q, %v", out, err)
	}

	if _, held, err := daemonlock.Probe(filepath.Join(dataDir, "daemon.lock")); err != nil || held {
		t.Errorf("after stop: held %v, err %v; want the lock free", held, err)
	}

	if out, err := run("logs", "-n", "1"); err != nil || !strings.Contains(out, `"message":"daemon stopped"`) {
		t.Errorf("logs after stop: %q, %v; want the daemon's last line to say it stopped", out, err)
	}

	if out, err := run("stop"); err == nil || !strings.Contains(out, "daemon is not running") {
		t.Errorf("stop again: %q, %v; want it to fail saying no daemon runs", out, err)
	}
}
