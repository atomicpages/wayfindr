//go:build !windows

package upstream_test

import (
	"os"
	"syscall"
)

// alive reports whether process pid exists. Signal 0 checks for the process
// without signalling it; a child the manager has waited for no longer
// exists, not even as a zombie.
func alive(pid int) bool {
	p, err := os.FindProcess(pid)

	if err != nil {
		return false
	}

	return p.Signal(syscall.Signal(0)) == nil
}
