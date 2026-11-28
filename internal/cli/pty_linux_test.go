package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ptsName unlocks the terminal side of the pseudo-terminal whose other side
// is ptmx, and returns its path.
func ptsName(ptmx *os.File) (string, error) {
	fd := int(ptmx.Fd())

	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return "", fmt.Errorf("unlock: %w", err)
	}

	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)

	if err != nil {
		return "", fmt.Errorf("number: %w", err)
	}

	return fmt.Sprintf("/dev/pts/%d", n), nil
}
