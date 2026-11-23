package cli

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/selfupdate"
)

// fakeReleases is a GitHub releases root with one binary per tag. It
// records the paths asked for, so a test can tell a check from a download.
type fakeReleases struct {
	latest string
	bins   map[string]string // tag → binary content
	// wrongSum makes checksums.txt list a SHA-256 that is not the archive's,
	// as for a download corrupted or swapped after the release.
	wrongSum bool

	mu    sync.Mutex
	asked []string
}

// serve starts the fake laid out like GitHub: /releases/latest redirects to
// /releases/tag/<latest>, and /releases/download/<tag>/<file> serves the
// archive for this OS and CPU and checksums.txt.
func (f *fakeReleases) serve(t *testing.T) selfupdate.Source {
	t.Helper()

	asset := selfupdate.AssetName(runtime.GOOS, selfupdate.HostArch(runtime.GOOS, runtime.GOARCH))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /releases/latest", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		http.Redirect(w, r, "/releases/tag/"+f.latest, http.StatusFound)
	})
	mux.HandleFunc("GET /releases/download/{tag}/{file}", func(w http.ResponseWriter, r *http.Request) {
		f.record(r)
		bin, ok := f.bins[r.PathValue("tag")]

		if !ok {
			http.NotFound(w, r)

			return
		}

		archive := releaseArchive(t, asset, bin)

		switch r.PathValue("file") {
		case "checksums.txt":
			sum := sha256.Sum256(archive)

			if f.wrongSum {
				sum = sha256.Sum256([]byte("another archive"))
			}

			_, _ = fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(sum[:]), asset)
		case asset:
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, r)
		}
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return selfupdate.Source{BaseURL: srv.URL + "/releases", Client: srv.Client()}
}

func (f *fakeReleases) record(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, r.URL.Path)
}

// requests is how many requests the fake has answered.
func (f *fakeReleases) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.asked)
}

// downloaded reports whether any archive was asked for.
func (f *fakeReleases) downloaded() bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	for _, p := range f.asked {
		if strings.Contains(p, "/download/") && !strings.HasSuffix(p, "/checksums.txt") {
			return true
		}
	}

	return false
}

// releaseArchive packs bin as a release does: wayfindr in a .tar.gz, or
// wayfindr.exe in a .zip.
func releaseArchive(t *testing.T, asset, bin string) []byte {
	t.Helper()

	var buf bytes.Buffer

	if strings.HasSuffix(asset, ".zip") {
		zw := zip.NewWriter(&buf)
		w, err := zw.Create("wayfindr.exe")

		if err == nil {
			_, err = w.Write([]byte(bin))
		}

		if err == nil {
			err = zw.Close()
		}

		if err != nil {
			t.Fatal(err)
		}

		return buf.Bytes()
	}

	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := tw.WriteHeader(&tar.Header{Name: "wayfindr", Mode: 0o755, Size: int64(len(bin)), Typeflag: tar.TypeReg})

	if err == nil {
		_, err = tw.Write([]byte(bin))
	}

	if err == nil {
		err = tw.Close()
	}

	if err == nil {
		err = zw.Close()
	}

	if err != nil {
		t.Fatal(err)
	}

	return buf.Bytes()
}

// updateHost is a test host running wayfindr current from a binary in a
// temp dir, with releases from f and a service runner that records what it
// would run. The home directory is under the temp root, so no login service
// is installed unless the test installs one.
func updateHost(t *testing.T, current string, f *fakeReleases) (host Host, exe string, ran *[]string) {
	t.Helper()

	host, root := testHost(t)
	host.Env.HomeDir = func() (string, error) { return filepath.Join(root, "home"), nil }
	exe = filepath.Join(root, "bin", "wayfindr")

	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(exe, []byte("binary "+current), 0o755); err != nil { //nolint:gosec // G306: a fake executable
		t.Fatal(err)
	}

	calls := []string{}
	host.Version = current
	host.Releases = f.serve(t)
	host.Executable = func() (string, error) { return exe, nil }
	host.RunService = func(_ context.Context, name string, args ...string) error {
		calls = append(calls, strings.Join(append([]string{name}, args...), " "))

		return nil
	}

	return host, exe, &calls
}

func readExe(t *testing.T, exe string) string {
	t.Helper()
	b, err := os.ReadFile(exe) //nolint:gosec // inside the test's temp dir.

	if err != nil {
		t.Fatal(err)
	}

	return string(b)
}

func twoReleases() *fakeReleases {
	return &fakeReleases{
		latest: "v0.2.0",
		bins:   map[string]string{"v0.1.0": "binary v0.1.0", "v0.2.0": "binary v0.2.0", "v0.3.0-rc.1": "binary v0.3.0-rc.1"},
	}
}

func TestUpdateWhenUpToDateChangesNothing(t *testing.T) {
	for _, current := range []string{"v0.2.0", "v0.2.0-3-gabc1234"} {
		t.Run(current, func(t *testing.T) {
			f := twoReleases()
			host, exe, ran := updateHost(t, current, f)

			stdout, stderr, code := run(t, host, "update")

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			want := map[string]string{
				"v0.2.0":            "wayfindr v0.2.0 is up to date\n",
				"v0.2.0-3-gabc1234": "wayfindr v0.2.0-3-gabc1234 is newer than v0.2.0\n",
			}[current]

			if stdout != want {
				t.Errorf("stdout = %q, want %q", stdout, want)
			}

			if got := readExe(t, exe); got != "binary "+current || f.downloaded() || len(*ran) != 0 {
				t.Errorf("binary %q, downloaded %v, ran %q; want nothing changed", got, f.downloaded(), *ran)
			}
		})
	}
}

func TestUpdateInstallsTheLatestRelease(t *testing.T) {
	for _, current := range []string{"v0.1.0", "dev"} {
		t.Run(current, func(t *testing.T) {
			host, exe, ran := updateHost(t, current, twoReleases())

			stdout, stderr, code := run(t, host, "update")

			if code != 0 {
				t.Fatalf("exit code = %d; stderr: %s", code, stderr)
			}

			if got := readExe(t, exe); got != "binary v0.2.0" {
				t.Errorf("binary = %q, want the v0.2.0 release", got)
			}

			// No service is installed and no daemon runs: nothing to restart.
			want := fmt.Sprintf("Updated wayfindr %s → v0.2.0 at %s\n", current, exe)

			if stdout != want || len(*ran) != 0 {
				t.Errorf("stdout = %q, ran %q; want %q and no service commands", stdout, *ran, want)
			}
		})
	}
}

func TestUpdateCheck(t *testing.T) {
	cases := []struct {
		name    string
		current string
		args    []string
		code    int
		stdout  string
	}{
		{"behind", "v0.1.0", nil, 2, "wayfindr v0.1.0 → v0.2.0 available; run `wayfindr update` to install it\n"},
		{"up to date", "v0.2.0", nil, 0, "wayfindr v0.2.0 is up to date\n"},
		{"newer", "v0.2.0-1-gabc1234", nil, 0, "wayfindr v0.2.0-1-gabc1234 is newer than v0.2.0\n"},
		{"named newer", "v0.2.0", []string{"0.3.0-rc.1"}, 2, "wayfindr v0.2.0 → v0.3.0-rc.1 available; run `wayfindr update v0.3.0-rc.1` to install it\n"},
		{"named older", "v0.2.0", []string{"v0.1.0"}, 0, "wayfindr v0.2.0 is newer than v0.1.0\n"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := twoReleases()
			host, exe, _ := updateHost(t, tc.current, f)

			stdout, stderr, code := run(t, host, append([]string{"update", "--check"}, tc.args...)...)

			if code != tc.code || stdout != tc.stdout || stderr != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit %d, stdout %q, no stderr", code, stdout, stderr, tc.code, tc.stdout)
			}

			if f.downloaded() || readExe(t, exe) != "binary "+tc.current {
				t.Error("--check downloaded an archive or changed the binary")
			}
		})
	}
}

func TestUpdateCheckJSON(t *testing.T) {
	host, _, _ := updateHost(t, "v0.1.0", twoReleases())

	stdout, stderr, code := run(t, host, "update", "--check", "--json")

	if code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr: %s", code, stderr)
	}

	var got map[string]any

	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
	}

	want := map[string]any{"current": "v0.1.0", "latest": "v0.2.0", "update_available": true}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("JSON differs (-want +got):\n%s", diff)
	}
}

// --json is only for --check: the install prints text, so a script must not
// take it for JSON. It fails before anything is asked of GitHub.
func TestUpdateJSONNeedsCheck(t *testing.T) {
	f := twoReleases()
	host, exe, _ := updateHost(t, "v0.1.0", f)

	_, stderr, code := run(t, host, "update", "--json")

	if code != 1 || !strings.Contains(stderr, `--json is supported by "wayfindr update" only with --check`) {
		t.Errorf("exit %d, stderr %q; want 1 and the --check requirement", code, stderr)
	}

	if n := f.requests(); n != 0 || readExe(t, exe) != "binary v0.1.0" {
		t.Errorf("%d requests; want none and the binary unchanged", n)
	}
}

// A named version is installed as asked, even an older one, with a pointer
// to the storage downgrade caveat.
func TestUpdateToAnOlderVersionWarnsAboutTheStore(t *testing.T) {
	host, exe, _ := updateHost(t, "v0.2.0", twoReleases())

	stdout, stderr, code := run(t, host, "update", "0.1.0")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if got := readExe(t, exe); got != "binary v0.1.0" {
		t.Errorf("binary = %q, want the v0.1.0 release", got)
	}

	want := fmt.Sprintf("Updated wayfindr v0.2.0 → v0.1.0 at %s\n", exe) +
		"Note: v0.1.0 is older than v0.2.0. An older binary can't open a store that a newer one has migrated: " +
		"https://atomicpages.github.io/wayfindr/guide/storage#upgrades-and-downgrades\n"

	if stdout != want {
		t.Errorf("stdout = %q, want %q", stdout, want)
	}
}

func TestUpdateToTheVersionInstalledDoesNothing(t *testing.T) {
	f := twoReleases()
	host, exe, _ := updateHost(t, "v0.2.0", f)

	stdout, _, code := run(t, host, "update", "v0.2.0")

	if code != 0 || stdout != "wayfindr v0.2.0 is already installed; nothing to do\n" {
		t.Errorf("exit %d, stdout %q; want 0 and already installed", code, stdout)
	}

	if f.downloaded() || readExe(t, exe) != "binary v0.2.0" {
		t.Error("downloaded an archive or changed the binary")
	}
}

func TestUpdateRejectsAVersionThatIsNotSemver(t *testing.T) {
	host, _, _ := updateHost(t, "v0.1.0", twoReleases())

	_, stderr, code := run(t, host, "update", "latest")

	if code != 1 || !strings.Contains(stderr, `invalid version "latest"`) {
		t.Errorf("exit %d, stderr %q; want 1 and the invalid version", code, stderr)
	}
}

// An archive that doesn't match checksums.txt is never written: the binary
// stays byte for byte as it was, and no temporary file is left beside it.
func TestUpdateWithAChecksumMismatchChangesNothing(t *testing.T) {
	f := twoReleases()
	f.wrongSum = true
	host, exe, ran := updateHost(t, "v0.1.0", f)

	_, stderr, code := run(t, host, "update")

	if code != 1 || !strings.Contains(stderr, "download wayfindr v0.2.0: checksum mismatch for ") {
		t.Errorf("exit %d, stderr %q; want 1 and the checksum mismatch", code, stderr)
	}

	if got := readExe(t, exe); got != "binary v0.1.0" {
		t.Errorf("binary = %q, want it unchanged", got)
	}

	entries, err := os.ReadDir(filepath.Dir(exe))

	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 || len(*ran) != 0 {
		t.Errorf("%d entries beside the binary, ran %q; want only the binary and no service commands", len(entries), *ran)
	}
}

func TestUpdateOfAMissingReleaseNamesIt(t *testing.T) {
	host, exe, _ := updateHost(t, "v0.1.0", twoReleases())

	_, stderr, code := run(t, host, "update", "v9.9.9")

	if code != 1 || !strings.Contains(stderr, "download wayfindr v9.9.9: GET ") {
		t.Errorf("exit %d, stderr %q; want 1 naming v9.9.9", code, stderr)
	}

	if readExe(t, exe) != "binary v0.1.0" {
		t.Error("the binary changed")
	}
}

func TestUpdateNoRestartRunsNoServiceCommands(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, exe, ran := updateHost(t, "v0.1.0", twoReleases())

	if _, stderr, code := run(t, host, "service", "install"); code != 0 {
		t.Fatalf("service install: exit %d; stderr: %s", code, stderr)
	}

	*ran = (*ran)[:0]

	stdout, stderr, code := run(t, host, "update", "--no-restart")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if want := fmt.Sprintf("Updated wayfindr v0.1.0 → v0.2.0 at %s\n", exe); stdout != want || len(*ran) != 0 {
		t.Errorf("stdout = %q, ran %q; want %q and no service commands", stdout, *ran, want)
	}
}

// An installed login service is reloaded, not stopped: it restarts only a
// daemon that fails, so a stop would leave it down until the next login.
func TestUpdateRestartsAnInstalledService(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, _, ran := updateHost(t, "v0.1.0", twoReleases())

	if _, stderr, code := run(t, host, "service", "install"); code != 0 {
		t.Fatalf("service install: exit %d; stderr: %s", code, stderr)
	}

	*ran = (*ran)[:0]

	stdout, stderr, code := run(t, host, "update")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if !strings.HasSuffix(stdout, "\nRestarted the login service.\n") {
		t.Errorf("stdout = %q, want the service restarted", stdout)
	}

	last := map[string]string{
		"darwin": "launchctl bootstrap gui/",
		"linux":  "systemctl --user restart wayfindr.service",
	}[runtime.GOOS]

	if len(*ran) == 0 || !strings.HasPrefix((*ran)[len(*ran)-1], last) {
		t.Errorf("ran %q, want the unit loaded again, ending with %q", *ran, last)
	}
}

// A restart that fails after the swap says the binary was updated and how
// to finish by hand.
func TestUpdateWhoseRestartFailsSaysTheBinaryWasUpdated(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, exe, _ := updateHost(t, "v0.1.0", twoReleases())

	if _, stderr, code := run(t, host, "service", "install"); code != 0 {
		t.Fatalf("service install: exit %d; stderr: %s", code, stderr)
	}

	host.RunService = func(context.Context, string, ...string) error {
		return fmt.Errorf("Failed to connect to bus: No medium found")
	}

	_, stderr, code := run(t, host, "update")

	want := fmt.Sprintf("updated wayfindr to v0.2.0 at %s, but the login service did not restart", exe)

	if code != 1 || !strings.Contains(stderr, want) || !strings.Contains(stderr, "Restart it with `wayfindr service install`") {
		t.Errorf("exit %d, stderr %q; want 1, %q, and the manual step", code, stderr, want)
	}

	if readExe(t, exe) != "binary v0.2.0" {
		t.Error("the new binary is not in place")
	}
}

// With no service, a running daemon is stopped so the next client starts
// the new binary.
func TestUpdateStopsTheRunningDaemon(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the lock holder helper stops by SIGTERM; daemonctl's Stop tests are Unix-only too")
	}

	host, exe, ran := updateHost(t, "v0.1.0", twoReleases())
	root := filepath.Dir(filepath.Dir(exe))
	holder := startLockHolder(t, filepath.Join(root, "data", "wayfindr", "daemon.lock"), "127.0.0.1:7777")

	stdout, stderr, code := run(t, host, "update")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	want := fmt.Sprintf("\nStopped the daemon (pid %d); the next client starts the new binary.\n", holder.Process.Pid)

	if !strings.HasSuffix(stdout, want) || len(*ran) != 0 {
		t.Errorf("stdout = %q, ran %q; want it to end %q with no service commands", stdout, *ran, want)
	}
}

// With a service installed, a daemon holding the lock (one started by hand,
// say) is stopped before the unit is reloaded: otherwise the unit's new
// daemon finds it running and exits cleanly, the service manager never
// starts it again, and the old binary keeps running outside the service
// (wayfindr-p9vn).
func TestUpdateUnderAServiceStopsTheDaemonHoldingTheLock(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("no service manager on " + runtime.GOOS)
	}

	host, exe, _ := updateHost(t, "v0.1.0", twoReleases())

	if _, stderr, code := run(t, host, "service", "install"); code != 0 {
		t.Fatalf("service install: exit %d; stderr: %s", code, stderr)
	}

	lockFile := filepath.Join(filepath.Dir(filepath.Dir(exe)), "data", "wayfindr", "daemon.lock")
	startLockHolder(t, lockFile, "127.0.0.1:7778")

	var ran []string
	loadedWhileHeld := false
	host.RunService = func(_ context.Context, name string, args ...string) error {
		if _, held, _ := daemonlock.Probe(lockFile); held {
			loadedWhileHeld = true
		}

		ran = append(ran, strings.Join(append([]string{name}, args...), " "))

		return nil
	}

	stdout, stderr, code := run(t, host, "update")

	if code != 0 {
		t.Fatalf("exit code = %d; stderr: %s", code, stderr)
	}

	if loadedWhileHeld {
		t.Error("the service manager ran while the old daemon still held the lock")
	}

	if _, held, err := daemonlock.Probe(lockFile); held || err != nil {
		t.Errorf("lock held = %v, %v after update; want the old daemon stopped", held, err)
	}

	last := map[string]string{
		"darwin": "launchctl bootstrap gui/",
		"linux":  "systemctl --user restart wayfindr.service",
	}[runtime.GOOS]

	if len(ran) == 0 || !strings.HasPrefix(ran[len(ran)-1], last) || !strings.HasSuffix(stdout, "\nRestarted the login service.\n") {
		t.Errorf("ran %q, stdout %q; want the unit reloaded, ending with %q, and the restart reported", ran, stdout, last)
	}
}

// sudoUser is the decision whether update runs as root on a user's behalf,
// whose daemon a restart as root would miss. It is tested directly because
// a test can't change its own euid; update skips the restart when it holds.
func TestSudoUser(t *testing.T) {
	cases := []struct {
		name     string
		euid     int
		sudoUser string
		want     string
		wantOK   bool
	}{
		{"root through sudo", 0, "alice", "alice", true},
		{"root without sudo", 0, "", "", false},
		{"root through sudo from root", 0, "root", "", false},
		{"a user", 501, "", "", false},
		{"a user with SUDO_USER left set", 501, "alice", "", false},
		{"windows", -1, "", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string {
				if k == "SUDO_USER" {
					return tc.sudoUser
				}

				return ""
			}

			if got, ok := sudoUser(tc.euid, getenv); got != tc.want || ok != tc.wantOK {
				t.Errorf("sudoUser(%d, SUDO_USER=%q) = %q, %v; want %q, %v", tc.euid, tc.sudoUser, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
