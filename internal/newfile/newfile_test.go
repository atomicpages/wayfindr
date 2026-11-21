package newfile_test

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/atomicpages/wayfindr/internal/newfile"
)

func TestCreateWritesDataWithTheGivenMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client-token")

	if err := newfile.Create(path, []byte("wf_token"), 0o600, path); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := os.ReadFile(filepath.Clean(path))

	if err != nil {
		t.Fatal(err)
	}

	if string(got) != "wf_token" {
		t.Errorf("content = %q, want %q", got, "wf_token")
	}

	if runtime.GOOS == "windows" {
		return // Windows has no Unix permission bits.
	}

	fi, err := os.Stat(path)

	if err != nil {
		t.Fatal(err)
	}

	if m := fi.Mode().Perm(); m != 0o600 {
		t.Errorf("mode = %v, want 0600", m)
	}
}

func TestCreatePinsTheModeAgainstTheUmask(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}

	// 0o666 is narrowed by any usual umask (022, 077); Create must not be.
	path := filepath.Join(t.TempDir(), "f")

	if err := newfile.Create(path, nil, 0o666, path); err != nil {
		t.Fatalf("Create: %v", err)
	}

	fi, err := os.Stat(path)

	if err != nil {
		t.Fatal(err)
	}

	if m := fi.Mode().Perm(); m != 0o666 {
		t.Errorf("mode = %v, want 0666", m)
	}
}

func TestCreateRefusesAnExistingFileAndLeavesItAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "master.key")

	if err := os.WriteFile(path, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := newfile.Create(path, []byte("new"), 0o600, "key file "+path)

	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("Create: err = %v, want fs.ErrExist", err)
	}

	if want := "create key file " + path + ":"; !strings.HasPrefix(err.Error(), want) {
		t.Errorf("err = %q, want it to start with %q", err, want)
	}

	if got, _ := os.ReadFile(filepath.Clean(path)); string(got) != "existing" {
		t.Errorf("content = %q, want it unchanged", got)
	}
}

func TestCreateFailsWhenTheDirectoryIsMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "f")

	err := newfile.Create(path, []byte("x"), 0o600, path)

	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Create: err = %v, want fs.ErrNotExist (callers create directories)", err)
	}
}

func TestReplaceKeepsTheMode(t *testing.T) {
	// Windows has no Unix permission bits to keep: 0400 there is the
	// read-only attribute, and Windows refuses to rename over a read-only
	// file at all.
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "wayfindr.toml")

	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Read-only: a mode CreateTemp's 0600 would lose.
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}

	if err := newfile.Replace(path, []byte("old"), []byte("new"), path); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	assertContent(t, path, "new")
	assertOnlyFiles(t, dir, "wayfindr.toml")

	fi, err := os.Stat(path)

	if err != nil {
		t.Fatal(err)
	}

	if m := fi.Mode().Perm(); m != 0o400 {
		t.Errorf("mode = %v, want 0400", m)
	}
}

// TestReplaceFollowsASymlink: a config linked in by a dotfile manager stays
// a link, and its target gets the new content.
func TestReplaceFollowsASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "wayfindr.toml")
	link := filepath.Join(dir, "wayfindr.toml")

	if err := os.Mkdir(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(target, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := newfile.Replace(link, []byte("old"), []byte("new"), link); err != nil {
		t.Fatalf("Replace: %v", err)
	}

	fi, err := os.Lstat(link)

	if err != nil {
		t.Fatal(err)
	}

	if fi.Mode()&fs.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink", link)
	}

	assertContent(t, target, "new")
}

// TestReplaceRefusesAChangedFile: a write between the caller's read and
// Replace is kept, not overwritten, and no temporary file is left.
func TestReplaceRefusesAChangedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wayfindr.toml")

	if err := os.WriteFile(path, []byte("edited by hand"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := newfile.Replace(path, []byte("old"), []byte("new"), path)

	if !errors.Is(err, newfile.ErrChanged) {
		t.Fatalf("Replace = %v, want ErrChanged", err)
	}

	assertContent(t, path, "edited by hand")
	assertOnlyFiles(t, dir, "wayfindr.toml")
}

func TestReplaceFailsOnAMissingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wayfindr.toml")

	if err := newfile.Replace(path, nil, []byte("new"), path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Replace = %v, want fs.ErrNotExist", err)
	}

	assertOnlyFiles(t, dir)
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Clean(path))

	if err != nil {
		t.Fatal(err)
	}

	if string(got) != want {
		t.Errorf("%s = %q, want %q", path, got, want)
	}
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)

	if err != nil {
		t.Fatal(err)
	}

	var got []string

	for _, e := range entries {
		if !e.IsDir() {
			got = append(got, e.Name())
		}
	}

	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files in %s = %v, want %v", dir, got, want)
	}
}
