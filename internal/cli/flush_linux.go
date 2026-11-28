package cli

import "golang.org/x/sys/unix"

// discardTyped drops what was typed on the terminal fd and not yet read,
// as tcflush(fd, TCIFLUSH) does.
func discardTyped(fd int) error {
	return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH)
}
