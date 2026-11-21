//go:build windows

package daemonlock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockOffsetHigh places the locked byte far past the holder's few bytes of
// JSON: Windows byte-range locks are mandatory, so a lock over the content
// would keep a second daemon and the shim from reading who holds it.
const lockOffsetHigh = 0x40000000

// tryLock takes LockFileEx on one byte of f without blocking, exclusive or
// shared, and reports false when another handle holds a conflicting lock.
// The lock belongs to the handle and goes with its close.
func tryLock(f *os.File, exclusive bool) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)

	if exclusive {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}

	ol := &windows.Overlapped{OffsetHigh: lockOffsetHigh}
	err := windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, 1, 0, ol)

	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}

// unlock drops the lock on f. Windows releases a closed handle's locks only
// eventually, so the holder unlocks before it closes.
func unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{OffsetHigh: lockOffsetHigh})
}
