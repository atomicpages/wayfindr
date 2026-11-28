package cli

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ptsName grants and unlocks the terminal side of the pseudo-terminal
// whose other side is ptmx, and returns its path. Its minor device number
// is ptmx's, so the path needs no TIOCPTYGNAME buffer.
func ptsName(ptmx *os.File) (string, error) {
	fd := int(ptmx.Fd())

	if err := unix.IoctlSetInt(fd, unix.TIOCPTYGRANT, 0); err != nil {
		return "", fmt.Errorf("grant: %w", err)
	}

	if err := unix.IoctlSetInt(fd, unix.TIOCPTYUNLK, 0); err != nil {
		return "", fmt.Errorf("unlock: %w", err)
	}

	var st unix.Stat_t

	if err := unix.Fstat(fd, &st); err != nil {
		return "", fmt.Errorf("stat: %w", err)
	}

	return fmt.Sprintf("/dev/ttys%03d", unix.Minor(uint64(st.Rdev))), nil //nolint:gosec // G115: a device number is not negative
}
