package selfupdate_test

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/selfupdate"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a stand-in executable
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path) //nolint:gosec // G304: a path in the test's temp dir
	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

func TestReplaceUnix(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exe := filepath.Join(dir, "wayfindr")
	writeFile(t, exe, "old binary")

	if err := os.Chmod(exe, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := selfupdate.Replace(exe, []byte("new binary"), "linux"); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	if got := readFile(t, exe); got != "new binary" {
		t.Errorf("exe content = %q, want %q", got, "new binary")
	}

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(exe)
		if err != nil {
			t.Fatal(err)
		}

		if got := fi.Mode().Perm(); got != 0o755 {
			t.Errorf("exe mode = %v, want 0755", got)
		}
	}

	if diff := cmp.Diff([]string{"wayfindr"}, dirNames(t, dir)); diff != "" {
		t.Errorf("dir entries mismatch, want no temp leftovers (-want +got):\n%s", diff)
	}
}

// Temporary files left by a run that was killed before its rename are
// removed by the next one, and nothing else in the directory is touched.
func TestReplaceRemovesLeftoverTempFiles(t *testing.T) {
	t.Parallel()

	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			exe := filepath.Join(dir, "wayfindr")
			writeFile(t, exe, "old binary")
			writeFile(t, filepath.Join(dir, ".wayfindr.123456.new"), "partial binary")
			writeFile(t, filepath.Join(dir, ".wayfindr.987654.new"), "another partial binary")
			writeFile(t, filepath.Join(dir, "wayfindr.new"), "not ours")

			if err := selfupdate.Replace(exe, []byte("new binary"), goos); err != nil {
				t.Fatalf("Replace() error = %v", err)
			}

			if got := readFile(t, exe); got != "new binary" {
				t.Errorf("exe content = %q, want %q", got, "new binary")
			}

			want := []string{"wayfindr", "wayfindr.new"}
			if goos == "windows" {
				want = []string{"wayfindr", "wayfindr.new", "wayfindr.old"}
			}

			if diff := cmp.Diff(want, dirNames(t, dir)); diff != "" {
				t.Errorf("dir entries mismatch, want the leftovers gone (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReplaceUnixFailureLeavesNoTemp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	// A non-empty directory where the binary should be: the rename fails.
	exe := filepath.Join(dir, "wayfindr")
	if err := os.Mkdir(exe, 0o750); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(exe, "keep"), "x")

	err := selfupdate.Replace(exe, []byte("new binary"), "linux")
	if err == nil || !strings.Contains(err.Error(), exe) {
		t.Fatalf("Replace() error = %v, want one naming %s", err, exe)
	}

	if diff := cmp.Diff([]string{"wayfindr"}, dirNames(t, dir)); diff != "" {
		t.Errorf("dir entries mismatch, want no temp leftovers (-want +got):\n%s", diff)
	}
}

func TestReplaceUnwritableDir(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory permission check that applies to this user")
	}

	for _, goos := range []string{"darwin", "windows"} {
		t.Run(goos, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			exe := filepath.Join(dir, "wayfindr")
			writeFile(t, exe, "old binary")

			if err := os.Chmod(dir, 0o555); err != nil { //nolint:gosec // G302: a read-only test dir
				t.Fatal(err)
			}

			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) }) //nolint:gosec // G302: restore so TempDir can clean up

			err := selfupdate.Replace(exe, []byte("new binary"), goos)
			if err == nil {
				t.Fatal("Replace() into a read-only dir succeeded, want an error")
			}

			for _, want := range []string{dir, "permission", "installer"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Replace() error = %v, want it to contain %q", err, want)
				}
			}

			if got := readFile(t, exe); got != "old binary" {
				t.Errorf("exe content = %q, want it unchanged", got)
			}

			if diff := cmp.Diff([]string{"wayfindr"}, dirNames(t, dir)); diff != "" {
				t.Errorf("dir entries mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestReplaceWindows(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exe := filepath.Join(dir, "wayfindr.exe")
	writeFile(t, exe, "running binary")
	// Copies left by two earlier updates, neither in use any more.
	writeFile(t, exe+".old", "older binary")
	writeFile(t, exe+".old-0123abcd", "oldest binary")
	writeFile(t, filepath.Join(dir, "other.exe.old"), "not ours")

	if err := selfupdate.Replace(exe, []byte("new binary"), "windows"); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	if got := readFile(t, exe); got != "new binary" {
		t.Errorf("exe content = %q, want %q", got, "new binary")
	}

	if got := readFile(t, exe+".old"); got != "running binary" {
		t.Errorf("exe.old content = %q, want the binary that was running", got)
	}

	want := []string{"other.exe.old", "wayfindr.exe", "wayfindr.exe.old"}
	if diff := cmp.Diff(want, dirNames(t, dir)); diff != "" {
		t.Errorf("dir entries mismatch (-want +got):\n%s", diff)
	}
}

func TestReplaceWindowsOldStillInUse(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exe := filepath.Join(dir, "wayfindr.exe")
	writeFile(t, exe, "running binary")

	// exe.old cannot be removed, as when a process still runs it on Windows:
	// here it is a non-empty directory.
	if err := os.Mkdir(exe+".old", 0o750); err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(exe+".old", "busy"), "x")

	if err := selfupdate.Replace(exe, []byte("new binary"), "windows"); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	if got := readFile(t, exe); got != "new binary" {
		t.Errorf("exe content = %q, want %q", got, "new binary")
	}

	names := dirNames(t, dir)
	unique := regexp.MustCompile(`^wayfindr\.exe\.old-[0-9a-f]{32}$`)

	i := slices.IndexFunc(names, unique.MatchString)
	if i < 0 {
		t.Fatalf("dir entries = %v, want one wayfindr.exe.old-<hex>", names)
	}

	if got := readFile(t, filepath.Join(dir, names[i])); got != "running binary" {
		t.Errorf("%s content = %q, want the binary that was running", names[i], got)
	}

	if len(names) != 3 {
		t.Errorf("dir entries = %v, want wayfindr.exe, wayfindr.exe.old, and one .old-<hex>", names)
	}
}

func TestReplaceWindowsFreshInstall(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	exe := filepath.Join(dir, "wayfindr.exe")

	if err := selfupdate.Replace(exe, []byte("new binary"), "windows"); err != nil {
		t.Fatalf("Replace() error = %v", err)
	}

	if diff := cmp.Diff([]string{"wayfindr.exe"}, dirNames(t, dir)); diff != "" {
		t.Errorf("dir entries mismatch (-want +got):\n%s", diff)
	}
}
