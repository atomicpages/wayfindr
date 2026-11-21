package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/paths"
)

func TestLogsPrintsTheLastLinesOfTheDaemonLog(t *testing.T) {
	host, _ := testHost(t)
	writeFile(t, resolvePaths(t, host).LogFile(), "one\ntwo\nthree\n")
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"logs", "-n", "2"}, &stdout, &stderr, host); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	if diff := cmp.Diff("two\nthree\n", stdout.String()); diff != "" {
		t.Errorf("stdout (-want +got):\n%s", diff)
	}
}

func TestLogsWithoutALogSaysHowOneIsWritten(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"logs"}, &stdout, &stderr, host); code == 0 {
		t.Fatal("exit code = 0, want non-zero: there is no log")
	}

	if msg := stderr.String(); !strings.Contains(msg, "no log at "+resolvePaths(t, host).LogFile()) || !strings.Contains(msg, "`wayfindr start`") {
		t.Errorf("stderr = %q, want it to name the log and how a daemon writes it", msg)
	}
}

func TestStopWithNoDaemonSaysSo(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"stop"}, &stdout, &stderr, host); code == 0 {
		t.Fatal("exit code = 0, want non-zero: no daemon runs")
	}

	if !strings.Contains(stderr.String(), "daemon is not running") {
		t.Errorf("stderr = %q, want it to say the daemon is not running", stderr.String())
	}
}

func TestStartBeforeInitSaysToRunInit(t *testing.T) {
	host, _ := testHost(t)
	var stdout, stderr bytes.Buffer

	if code := RunWith([]string{"start"}, &stdout, &stderr, host); code == 0 {
		t.Fatal("exit code = 0, want non-zero: there is no config")
	}

	if !strings.Contains(stderr.String(), "run `wayfindr init` first") {
		t.Errorf("stderr = %q, want it to say to run init", stderr.String())
	}
}

// A daemon that already answers is reported, and none is started: a start
// here would run the test binary. `serve --detach`, start's old form, does
// the same.
func TestStartReportsADaemonThatAlreadyAnswers(t *testing.T) {
	for _, args := range [][]string{{"start"}, {"serve", "--detach"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			t.Cleanup(srv.Close)

			host, root := testHost(t)
			listen := srv.Listener.Addr().String()
			writeFile(t, resolvePaths(t, host).ConfigFile, fmt.Sprintf("[server]\nlisten = %q\n", listen))
			holdDaemonLock(t, root, listen)
			var stdout, stderr bytes.Buffer

			if code := RunWith(args, &stdout, &stderr, host); code != 0 {
				t.Fatalf("exit code = %d, want 0; stderr: %s", code, stderr.String())
			}

			if diff := cmp.Diff("Daemon already running at http://"+listen+"\n", stdout.String()); diff != "" {
				t.Errorf("stdout (-want +got):\n%s", diff)
			}
		})
	}
}

func resolvePaths(t *testing.T, host Host) paths.Paths {
	t.Helper()
	p, err := paths.Resolve(host.Env)

	if err != nil {
		t.Fatal(err)
	}

	return p
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
