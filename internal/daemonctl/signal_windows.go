//go:build windows

package daemonctl

import (
	"errors"
	"os"
)

// terminate kills the process: Windows has no SIGTERM, and the daemon is
// detached from every console, so no console control event reaches it.
// Its stdio connectors run in a job object that closes with it. A daemon
// that exited since the lock was read is already what the caller wants.
func terminate(pid int) error {
	p, err := os.FindProcess(pid)

	if err != nil {
		return nil //nolint:nilerr // no such process: it has exited
	}

	if err := p.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}

	return nil
}
