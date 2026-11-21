//go:build !windows

package daemonctl_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
)

// These tests stand in for the daemon with this test binary, run again as
// a helper process: one that holds the daemon lock until SIGTERM, or, when
// Ensure starts "the daemon" (this executable with `start`), one that exits
// at once, as a daemon with a broken config does.

const (
	holdLockEnv   = "DAEMONCTL_TEST_HOLD_LOCK"
	holdListenEnv = "DAEMONCTL_TEST_HOLD_LISTEN"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "start" {
		fmt.Fprintln(os.Stderr, "fake daemon: refusing to start")
		fmt.Fprintln(os.Stderr, "WAYFINDR_CONFIG="+os.Getenv("WAYFINDR_CONFIG"))
		fmt.Fprintln(os.Stderr, "WAYFINDR_DATA_DIR="+os.Getenv("WAYFINDR_DATA_DIR"))
		os.Exit(3)
	}

	if path := os.Getenv(holdLockEnv); path != "" {
		os.Exit(holdLock(path, os.Getenv(holdListenEnv)))
	}

	os.Exit(m.Run())
}

// holdLock takes the daemon lock as a daemon would, says so on stdout, and
// releases it on SIGTERM.
func holdLock(path, listen string) int {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	lock, err := daemonlock.Acquire(ctx, path, daemonlock.Holder{PID: os.Getpid(), Listen: listen})

	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return 1
	}

	fmt.Println("ready")
	<-ctx.Done()

	if err := lock.Release(); err != nil {
		return 1
	}

	return 0
}

// startHolder runs the lock-holding helper and returns once it holds the
// lock at path.
func startHolder(t *testing.T, path, listen string) *exec.Cmd {
	t.Helper()
	exe, err := os.Executable()

	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(context.WithoutCancel(t.Context()), exe) //nolint:gosec // G204: this test binary
	cmd.Env = append(os.Environ(), holdLockEnv+"="+path, holdListenEnv+"="+listen)
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()

	if err != nil {
		t.Fatal(err)
	}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})

	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("helper said %q, %v; want ready", line, err)
	}

	return cmd
}

func TestStopEndsTheLockHolderAndWaitsForTheLockToDrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.lock")
	holder := startHolder(t, path, "127.0.0.1:7777")

	h, err := daemonctl.Stop(t.Context(), path)

	if err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if h.PID != holder.Process.Pid {
		t.Errorf("stopped pid %d, want the holder's %d", h.PID, holder.Process.Pid)
	}

	if _, held, err := daemonlock.Probe(path); err != nil || held {
		t.Errorf("after Stop: held = %v, err = %v; want the lock free", held, err)
	}

	if err := holder.Wait(); err != nil {
		t.Errorf("holder exit: %v, want a clean exit on SIGTERM", err)
	}
}

func TestStopWithNoDaemonIsErrNotRunning(t *testing.T) {
	for name, path := range map[string]string{
		"no lock file":  filepath.Join(t.TempDir(), "daemon.lock"),
		"lock not held": writeLog(t, `{"pid":1,"listen":"127.0.0.1:7777"}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := daemonctl.Stop(t.Context(), path); !errors.Is(err, daemonctl.ErrNotRunning) {
				t.Errorf("Stop: %v, want ErrNotRunning", err)
			}
		})
	}
}

func TestEnsureFindsADaemonThatAnswers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	target := newTarget(t, srv.Listener.Addr().String())
	startHolder(t, target.LockFile, target.Listen)

	running, err := daemonctl.Ensure(t.Context(), target, daemonctl.StartOptions{})

	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	if running.Started || running.Holder.Listen != target.Listen || running.Holder.PID == 0 {
		t.Errorf("running = %+v, want the lock's holder, not started", running)
	}
}

// After server.listen is edited, the daemon on the data directory still
// holds the lock with the old address: Ensure names it rather than start a
// second daemon on the same store.
func TestEnsureReportsADaemonOnAnotherListenAddress(t *testing.T) {
	target := newTarget(t, freeAddr(t))
	startHolder(t, target.LockFile, "127.0.0.1:7777")

	_, err := daemonctl.Ensure(t.Context(), target, daemonctl.StartOptions{})

	if err == nil || !strings.Contains(err.Error(), "daemon already running at 127.0.0.1:7777") || !strings.Contains(err.Error(), "server.listen is now "+target.Listen) {
		t.Errorf("Ensure: %v, want it to name the daemon on the old address", err)
	}
}

// A process that answers /healthz on the listen address but does not hold
// the daemon lock is not the daemon (wayfindr-zvjw): the lock sits in the
// user's own data directory, so another user's process can't hold it.
// Ensure refuses it, so the shim never sends it the client token, and
// starts no daemon that could not bind the address.
func TestEnsureRefusesAListenerThatDoesNotHoldTheLock(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	target := newTarget(t, srv.Listener.Addr().String())

	_, err := daemonctl.Ensure(t.Context(), target, daemonctl.StartOptions{})

	if !errors.Is(err, daemonctl.ErrNotOurs) || !strings.Contains(err.Error(), target.Listen) {
		t.Fatalf("Ensure: %v, want ErrNotOurs naming %s", err, target.Listen)
	}

	if _, err := os.Stat(target.LogFile); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("daemon log: %v, want none: Ensure must start no daemon", err)
	}
}

// The daemon is checked on every new connection, not only the first
// (wayfindr-zvjw): when it exits and something else keeps its address, a
// client with a session dials again, and that dial is refused, so the next
// request never leaves with the client token.
func TestTransportChecksTheDaemonOnEveryDial(t *testing.T) {
	var sawToken atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.Header.Get("Authorization") != "" {
			sawToken.Store(true)
		}
	}))
	t.Cleanup(srv.Close)
	target := newTarget(t, srv.Listener.Addr().String())
	holder := startHolder(t, target.LockFile, target.Listen)
	client := &http.Client{Transport: target.Transport()}
	get := func() error {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, target.URL()+"/mcp", http.NoBody)

		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Authorization", "Bearer wf_token")
		resp, err := client.Do(req)

		if err == nil {
			_ = resp.Body.Close()
		}

		return err
	}

	if err := get(); err != nil {
		t.Fatalf("request to the daemon holding the lock: %v", err)
	}

	// The daemon exits and its address is someone else's; the client's
	// connection is gone with it.
	if err := holder.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}

	_ = holder.Wait()
	srv.CloseClientConnections()
	sawToken.Store(false)

	if err := get(); !errors.Is(err, daemonctl.ErrNotOurs) {
		t.Errorf("request after the daemon exited: %v, want ErrNotOurs", err)
	}

	if sawToken.Load() {
		t.Error("the new listener got the client token")
	}
}

// A daemon that exits at startup (a broken config, say) fails Ensure at
// once, pointing at the log, rather than after the start timeout.
func TestEnsureFailsAtOnceWhenTheDaemonItStartedExits(t *testing.T) {
	target := newTarget(t, freeAddr(t))
	begin := time.Now()

	_, err := daemonctl.Ensure(t.Context(), target, daemonctl.StartOptions{})

	if err == nil || !strings.Contains(err.Error(), "exited before it answered") || !strings.Contains(err.Error(), target.LogFile) {
		t.Fatalf("Ensure: %v, want it to say the daemon exited and name the log", err)
	}

	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Errorf("Ensure took %s, want it to fail before the start timeout", elapsed)
	}

	log, err := os.ReadFile(target.LogFile)

	if err != nil || !strings.Contains(string(log), "fake daemon: refusing to start") {
		t.Errorf("log = %q, %v; want the daemon's output", log, err)
	}
}

// The daemon Ensure starts uses the config file and data directory the
// target was resolved for, even when the starter's environment names
// others: they may have come from --config and --data-dir.
func TestEnsureStartsTheDaemonOnTheTargetsConfigAndDataDir(t *testing.T) {
	t.Setenv("WAYFINDR_CONFIG", "/elsewhere/wayfindr.toml")
	t.Setenv("WAYFINDR_DATA_DIR", "/elsewhere/data")
	target := newTarget(t, freeAddr(t))
	target.ConfigFile = filepath.Join(t.TempDir(), "flag.toml")
	target.DataDir = filepath.Dir(target.LogFile)

	if _, err := daemonctl.Ensure(t.Context(), target, daemonctl.StartOptions{}); err == nil {
		t.Fatal("Ensure: nil error, want the fake daemon's exit")
	}

	log, err := os.ReadFile(target.LogFile)

	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"WAYFINDR_CONFIG=" + target.ConfigFile + "\n", "WAYFINDR_DATA_DIR=" + target.DataDir + "\n"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("daemon log = %q, want it to show %q", log, want)
		}
	}
}

func newTarget(t *testing.T, listen string) daemonctl.Target {
	t.Helper()
	dir := t.TempDir()

	return daemonctl.Target{
		Listen:   listen,
		LockFile: filepath.Join(dir, "daemon.lock"),
		LogFile:  filepath.Join(dir, "wayfindr.log"),
		Log:      zerolog.Nop(),
	}
}

// freeAddr returns a loopback address nothing listens on.
func freeAddr(t *testing.T) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")

	if err != nil {
		t.Fatal(err)
	}

	addr := ln.Addr().String()

	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}

	return addr
}
