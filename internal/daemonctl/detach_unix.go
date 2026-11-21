//go:build !windows

package daemonctl

import "syscall"

// detached puts the daemon in a new session, without a controlling
// terminal, so the client's signals to the shim's process group and the
// terminal's hangup never reach it.
func detached() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
