//go:build !windows

package daemonlock

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// tryLock takes flock(2) on f without blocking, exclusive or shared, and
// reports false when another open file holds a conflicting one. flock
// belongs to the open file, not the process, so two opens in one process
// conflict too, and the lock goes with the last descriptor's close.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	how := unix.LOCK_SH

	if exclusive {
		how = unix.LOCK_EX
	}

	for {
		err := unix.Flock(int(f.Fd()), how|unix.LOCK_NB) //nolint:gosec // G115: a file descriptor fits in an int

		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, unix.EINTR):
			continue
		case errors.Is(err, unix.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

// unlock drops the lock on f, as closing it would.
func unlock(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:gosec // G115: a file descriptor fits in an int
}
