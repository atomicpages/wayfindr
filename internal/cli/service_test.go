package cli

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/daemonlock"
)

// A `start --foreground` that finds another daemon holding the lock exits 0
// (wayfindr-p9vn), whatever server.listen that daemon has. The login
// service runs it: a non-zero exit there makes the service manager start
// it again, every few seconds, for ever. `wayfindr start` already treats a
// running daemon as success.
func TestStartForegroundExitsZeroWhenADaemonAlreadyRuns(t *testing.T) {
	const listen = "127.0.0.1:1"

	for name, holder := range map[string]string{"same listen": listen, "another listen": "127.0.0.1:7777"} {
		t.Run(name, func(t *testing.T) {
			host, root := testHost(t)
			writeHostFiles(t, root, listen)
			holdDaemonLock(t, root, holder)

			_, stderr, code := run(t, host, "start", "--foreground")

			if code != 0 {
				t.Errorf("exit code = %d, want 0; stderr: %s", code, stderr)
			}
		})
	}
}

// service install first stops the daemon that holds the lock
// (wayfindr-p9vn), so the daemon the service starts takes it: otherwise
// that one finds the other running and exits, and the service manager
// starts it again, for ever.
func TestServiceInstallStopsTheRunningDaemonFirst(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, root := testHost(t)
	host.Env.HomeDir = func() (string, error) { return filepath.Join(root, "home"), nil }
	lockFile := filepath.Join(root, "data", "wayfindr", "daemon.lock")
	holder := startLockHolder(t, lockFile, "127.0.0.1:7777")
	loadedWhileHeld := false
	host.RunService = func(context.Context, string, ...string) error {
		if _, held, _ := daemonlock.Probe(lockFile); held {
			loadedWhileHeld = true
		}

		return nil
	}

	stdout, stderr, code := run(t, host, "service", "install")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if loadedWhileHeld {
		t.Error("the service manager ran while the old daemon still held the lock")
	}

	if want := fmt.Sprintf("Stopped the daemon (pid %d)", holder.Process.Pid); !strings.Contains(stdout, want) {
		t.Errorf("stdout = %q, want it to say %q", stdout, want)
	}
}

// With no daemon running, service install stops nothing and says nothing
// about it.
func TestServiceInstallWithNoDaemonStopsNothing(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, root := testHost(t)
	host.Env.HomeDir = func() (string, error) { return filepath.Join(root, "home"), nil }
	host.RunService = func(context.Context, string, ...string) error { return nil }

	stdout, stderr, code := run(t, host, "service", "install")

	if code != 0 || strings.Contains(stdout, "Stopped") {
		t.Errorf("exit %d, stdout %q, stderr %q; want success with nothing stopped", code, stdout, stderr)
	}
}

// When the service manager fails after the running daemon was stopped (no
// user bus over SSH, say), the error says the daemon is stopped and how to
// start it again (wayfindr-p9vn).
func TestServiceInstallThatFailsAfterStoppingSaysSo(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, root := testHost(t)
	host.Env.HomeDir = func() (string, error) { return filepath.Join(root, "home"), nil }
	startLockHolder(t, filepath.Join(root, "data", "wayfindr", "daemon.lock"), "127.0.0.1:7777")
	host.RunService = func(_ context.Context, _ string, args ...string) error {
		if slices.Contains(args, "print") {
			return errors.New("not loaded")
		}

		return errors.New("Failed to connect to bus: No medium found")
	}

	_, stderr, code := run(t, host, "service", "install")

	if code == 0 || !strings.Contains(stderr, "The daemon that was running is stopped: start it with `wayfindr start`") {
		t.Errorf("exit %d, stderr %q; want the failure and how to start the daemon again", code, stderr)
	}
}
