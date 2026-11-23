package selfupdate

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Replace installs bin as the executable at exe, which should already have
// its symlinks resolved. It writes bin to a temporary file in exe's
// directory, so the swap never crosses a file system, then swaps it in:
//
//   - On Unix (any goos but "windows") it renames over exe, which is atomic;
//     a running daemon keeps the old binary until it restarts.
//   - On Windows a running exe cannot be overwritten but can be renamed, so,
//     as install.ps1 does, Replace removes old copies left by earlier updates
//     (exe.old*, skipping any still in use), moves exe aside to exe.old (or
//     exe.old-<random hex> when exe.old is still there), and renames the new
//     file in. If that last rename fails it moves the old one back.
//
// goos picks the scheme, so tests can run the Windows one anywhere. On any
// failure the temporary file is removed and exe is left as it was. Temporary
// files that an earlier, killed run left behind are removed first, as far as
// they can be.
func Replace(exe string, bin []byte, goos string) (err error) {
	dir := filepath.Dir(exe)

	removeMatching(dir, func(name string) bool {
		return strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, tempSuffix)
	})

	f, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			return fmt.Errorf("cannot write to %s: re-run with permission to write there (for example with sudo), or reinstall with the installer: %w", dir, err)
		}

		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}

	tmp := f.Name()

	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()

	if err := writeExecutable(f, bin); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}

	if goos == "windows" {
		return swapWindows(tmp, exe)
	}

	if err := os.Rename(tmp, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}

	return nil
}

// The new binary is written to a temporary file named tempPrefix, random
// digits, then tempSuffix, in exe's directory.
const (
	tempPrefix = ".wayfindr."
	tempSuffix = ".new"
)

// writeExecutable writes bin to f, makes it executable, flushes it to disk,
// and closes it, so a crash after the rename never leaves a partial binary.
func writeExecutable(f *os.File, bin []byte) error {
	_, err := f.Write(bin)
	if err == nil {
		// The installed binary must be runnable by everyone, as the
		// installers leave it.
		err = f.Chmod(0o755) //nolint:gosec // G302: an executable is 0755, like the installers make it
	}

	if err == nil {
		err = f.Sync()
	}

	if cerr := f.Close(); err == nil {
		err = cerr
	}

	return err
}

// swapWindows moves exe aside and renames tmp into its place.
func swapWindows(tmp, exe string) error {
	removeOldCopies(exe)

	old := ""

	if _, err := os.Lstat(exe); err == nil {
		old = exe + ".old"
		if _, err := os.Lstat(old); err == nil {
			// An earlier copy is still in use, so it could not be removed.
			suffix, err := randomHex()
			if err != nil {
				return fmt.Errorf("move %s aside: %w", exe, err)
			}

			old = exe + ".old-" + suffix
		}

		if err := os.Rename(exe, old); err != nil {
			return fmt.Errorf("move %s aside: %w", exe, err)
		}
	}

	if err := os.Rename(tmp, exe); err != nil {
		err = fmt.Errorf("replace %s: %w", exe, err)
		if old != "" {
			if rerr := os.Rename(old, exe); rerr != nil {
				err = errors.Join(err, fmt.Errorf("restore %s from %s: %w", exe, old, rerr))
			}
		}

		return err
	}

	return nil
}

// removeOldCopies deletes exe.old* left by earlier updates. A copy that a
// process still runs cannot be deleted on Windows; it stays until the next
// update.
func removeOldCopies(exe string) {
	prefix := filepath.Base(exe) + ".old"

	removeMatching(filepath.Dir(exe), func(name string) bool { return strings.HasPrefix(name, prefix) })
}

// removeMatching deletes the entries of dir whose names match, as a best
// effort: what can't be listed or deleted stays, so errors are ignored.
func removeMatching(dir string, match func(name string) bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, e := range entries {
		if match(e.Name()) {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func randomHex() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return hex.EncodeToString(b), nil
}
