//go:build !windows

package httpd

import "syscall"

// errAddrInUse is the errno a listen on a taken address fails with.
const errAddrInUse = syscall.EADDRINUSE
