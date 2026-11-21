// Package newfile writes the files Wayfindr owns: Create for the ones it
// writes once and never overwrites (the default config, the client token,
// the master key file), and Replace for the config edits of `wayfindr
// connector`. It is a stdlib-only leaf.
package newfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Create writes data to a new file at path with mode perm. It fails, with an
// error wrapping fs.ErrExist, if path exists, so it never overwrites. The
// parent directory must exist. On any failure after the file is created it
// removes the file, so no partial file is left behind.
//
// name is how errors refer to the file, for example the path itself or
// "key file " + path.
func Create(path string, data []byte, perm os.FileMode, name string) (err error) {
	f, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)

	if err != nil {
		return fmt.Errorf("create %s: %w", name, err)
	}

	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("close %s: %w", name, cerr)
		}

		if err != nil {
			_ = os.Remove(path)
		}
	}()

	// Chmod pins the mode even under an unusual umask.
	if err := f.Chmod(perm); err != nil {
		return fmt.Errorf("chmod %s: %w", name, err)
	}

	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}

	return nil
}

// ErrChanged means the file no longer holds what the caller read from it:
// something else wrote it in between, and Replace left it alone.
var ErrChanged = errors.New("file changed since it was read")

// Replace overwrites the file at path with data, atomically: it writes a
// temporary file in the same directory with the file's mode, syncs it, and
// renames it over the file, so a reader (the daemon's config watcher) sees
// the old content or the new, never half of it. A symlink at path is
// followed, and its target replaced, so the link survives.
//
// expect is what the caller read from the file. If the file no longer holds
// it, Replace fails with ErrChanged and writes nothing. The check and the
// rename are not one step, but the gap is as small as the stdlib allows.
//
// name is how errors refer to the file. On any failure, the temporary file
// is removed.
func Replace(path string, expect, data []byte, name string) (err error) {
	target, err := filepath.EvalSymlinks(path)

	if err != nil {
		return fmt.Errorf("resolve %s: %w", name, err)
	}

	fi, err := os.Stat(target)

	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*.tmp")

	if err != nil {
		return fmt.Errorf("create temp for %s: %w", name, err)
	}

	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()

	if err := tmp.Chmod(fi.Mode().Perm()); err != nil {
		return fmt.Errorf("chmod temp for %s: %w", name, err)
	}

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp for %s: %w", name, err)
	}

	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp for %s: %w", name, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp for %s: %w", name, err)
	}

	current, err := os.ReadFile(target) //nolint:gosec // the caller's own file

	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	if !bytes.Equal(current, expect) {
		return fmt.Errorf("replace %s: %w", name, ErrChanged)
	}

	if err := os.Rename(tmp.Name(), target); err != nil {
		return fmt.Errorf("replace %s: %w", name, err)
	}

	return nil
}
