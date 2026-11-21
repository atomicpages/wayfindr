package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"

	"github.com/atomicpages/wayfindr/internal/daemonlock"
)

// holdLockEnv names the daemon lock this test binary, run again as a
// helper, holds until SIGTERM, as a running daemon does; holdListenEnv is
// the server.listen it writes in.
const (
	holdLockEnv   = "WAYFINDR_CLI_TEST_HOLD_LOCK"
	holdListenEnv = "WAYFINDR_CLI_TEST_HOLD_LISTEN"
)

// TestMain refuses to run as a daemon. A command under test that finds no
// daemon starts this executable with `start --foreground`, as it would
// start wayfindr; running the tests again there would start more. Exiting
// at once is a daemon that can't start, which the commands report. `serve`,
// start's old name, is refused too. Run with holdLockEnv set, it holds the
// daemon lock instead (see startLockHolder).
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "start" || os.Args[1] == "serve") {
		fmt.Fprintln(os.Stderr, "the cli test binary is not a daemon")
		os.Exit(1)
	}

	if path := os.Getenv(holdLockEnv); path != "" {
		os.Exit(holdLock(path, os.Getenv(holdListenEnv)))
	}

	os.Exit(m.Run())
}

// holdLock takes the daemon lock at path as a daemon would, says so on
// stdout, and releases it on SIGTERM.
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

// startLockHolder runs this test binary as a process that holds the daemon
// lock at path for a daemon on listen, as a running daemon does, and
// returns once it holds it. `wayfindr stop` and `service install` signal
// it by the pid in the lock.
func startLockHolder(t *testing.T, path, listen string) *exec.Cmd {
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
		t.Fatalf("lock holder said %q, %v; want ready", line, err)
	}

	return cmd
}
