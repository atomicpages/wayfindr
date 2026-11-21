//go:build windows

package httpd

import "golang.org/x/sys/windows"

// errAddrInUse is the errno a listen on a taken address fails with. Winsock
// reports WSAEADDRINUSE, which syscall.EADDRINUSE (an errno Go invents on
// Windows) does not match.
const errAddrInUse = windows.WSAEADDRINUSE
