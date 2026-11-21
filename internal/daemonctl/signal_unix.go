//go:build !windows

package daemonctl

import (
	"errors"
	"syscall"
)

// terminate sends SIGTERM: the daemon drains requests in flight, stops its
// stdio connectors, and closes the store before it exits. A daemon that
// exited since the lock was read is already what the caller wants.
func terminate(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}

	return nil
}
