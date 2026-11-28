//go:build darwin || linux

package cli

import (
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

// openTerminal opens a pseudo-terminal, for a command that asks only on a
// terminal: tty is the terminal the command reads, and the test types into
// keys. Both are closed when the test ends.
func openTerminal(t *testing.T) (keys, tty *os.File) {
	t.Helper()
	keys, err := os.OpenFile("/dev/ptmx", os.O_RDWR|unix.O_NOCTTY, 0)

	if err != nil {
		t.Skipf("no pseudo-terminal: %v", err)
	}

	t.Cleanup(func() { _ = keys.Close() })
	name, err := ptsName(keys)

	if err != nil {
		t.Fatalf("set up the pseudo-terminal: %v", err)
	}

	tty, err = os.OpenFile(name, os.O_RDWR|unix.O_NOCTTY, 0) //nolint:gosec // G304: the path is the pseudo-terminal's own

	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}

	// The terminal echoes what is typed back to keys. Read it, or closing
	// tty waits for the echo to drain. The read ends when tty closes,
	// which the cleanups do before they close keys.
	drained := make(chan struct{})

	go func() {
		defer close(drained)

		_, _ = io.Copy(io.Discard, keys)
	}()

	t.Cleanup(func() {
		_ = tty.Close()
		<-drained
	})

	return keys, tty
}
