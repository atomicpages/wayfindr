package cli

import "golang.org/x/sys/unix"

// fread is FREAD of <sys/fcntl.h>, which x/sys/unix doesn't export: as
// TIOCFLUSH's argument, it flushes the input queue only.
const fread = 0x1

// discardTyped drops what was typed on the terminal fd and not yet read,
// as tcflush(fd, TCIFLUSH) does: libc's tcflush is this ioctl.
func discardTyped(fd int) error {
	return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread)
}
