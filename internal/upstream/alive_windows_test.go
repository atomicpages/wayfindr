//go:build windows

package upstream_test

import "golang.org/x/sys/windows"

// alive reports whether process pid exists and has not exited. Windows has
// no signal 0 (os.Process.Signal refuses it), so it opens the process and
// checks, without waiting, whether the handle is signalled, which happens
// when the process exits. A process with no handle left open is gone, and
// OpenProcess fails.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid)) //nolint:gosec // G115: a PID fits in a DWORD.

	if err != nil {
		return false
	}

	defer func() { _ = windows.CloseHandle(h) }()

	ev, err := windows.WaitForSingleObject(h, 0)

	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}
