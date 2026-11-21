//go:build windows

package upstream

import "golang.org/x/sys/windows"

// errConnRefused is the errno a TCP connect to a closed port fails with.
// Winsock reports WSAECONNREFUSED from connectex, which syscall.ECONNREFUSED
// (an errno Go invents on Windows) does not match.
const errConnRefused = windows.WSAECONNREFUSED
