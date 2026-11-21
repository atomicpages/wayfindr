//go:build windows

package daemonctl

import (
	"syscall"

	"golang.org/x/sys/windows"
)

// detached starts the daemon without a console and in its own process
// group, so closing the client's console or its Ctrl+C never reaches it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP,
		HideWindow:    true,
	}
}
