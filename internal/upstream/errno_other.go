//go:build !windows

package upstream

import "syscall"

// errConnRefused is the errno a TCP connect to a closed port fails with.
const errConnRefused = syscall.ECONNREFUSED
