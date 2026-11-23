package service_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/service"
)

// machine is a fake user account rooted at a temp dir: a home directory,
// an environment, and a command runner that records what would run
// instead of running it.
type machine struct {
	root string
	vars map[string]string
	// fail maps a command line (with <root> as a placeholder) to the error
	// the runner returns for it.
	fail map[string]error
	ran  []string
}

func newMachine(t *testing.T) *machine {
	t.Helper()

	return &machine{root: t.TempDir(), vars: map[string]string{}, fail: map[string]error{}}
}

// unixHost skips a test of the launchd or systemd backend on Windows. The
// CLI picks the backend by runtime.GOOS, so neither runs on Windows (see
// TestOtherOSIsNotSupported); simulated there, a Windows temp dir's paths
// would end up in a Unix unit file and in launchctl's arguments.
func unixHost(t *testing.T) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("launchd and systemd units exist only on macOS and Linux")
	}
}

func (m *machine) options(goos string) service.Options {
	return service.Options{
		GOOS:   goos,
		UID:    501,
		Binary: "/opt/wayfindr/bin/wayfindr",
		Env: paths.Env{
			Getenv:  func(k string) string { return m.vars[k] },
			HomeDir: func() (string, error) { return filepath.Join(m.root, "home"), nil },
		},
		Run: func(_ context.Context, name string, args ...string) error {
			line := strings.ReplaceAll(strings.Join(append([]string{name}, args...), " "), m.root, "<root>")
			m.ran = append(m.ran, line)

			return m.fail[line]
		},
	}
}

func assertGolden(t *testing.T, m *machine, unitFile, golden string) {
	t.Helper()
	got, err := os.ReadFile(unitFile) //nolint:gosec // inside the test's temp dir.

	if err != nil {
		t.Fatalf("read unit file: %v", err)
	}

	want, err := os.ReadFile(filepath.Join("testdata", golden)) //nolint:gosec // fixed test data.

	if err != nil {
		t.Fatal(err)
	}

	if diff := cmp.Diff(string(want), strings.ReplaceAll(string(got), m.root, "<root>")); diff != "" {
		t.Errorf("unit file differs from testdata/%s (-want +got):\n%s", golden, diff)
	}
}

func TestInstallWritesTheLaunchdAgent(t *testing.T) {
	unixHost(t)

	m := newMachine(t)

	res, err := service.Install(context.Background(), m.options("darwin"))

	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(m.root, "home", "Library", "LaunchAgents", "dev.wayfindr.daemon.plist")

	if res.UnitFile != want {
		t.Errorf("UnitFile = %q, want %q", res.UnitFile, want)
	}

	assertGolden(t, m, want, "launchd.plist.golden")
}

func TestInstallWritesTheSystemdUserUnit(t *testing.T) {
	unixHost(t)

	m := newMachine(t)

	res, err := service.Install(context.Background(), m.options("linux"))

	if err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(m.root, "home", ".config", "systemd", "user", "wayfindr.service")

	if res.UnitFile != want {
		t.Errorf("UnitFile = %q, want %q", res.UnitFile, want)
	}

	assertGolden(t, m, want, "systemd.service.golden")
}

// The unit carries the installer's path overrides and PATH, so the daemon
// resolves the same config and data dir and finds the same stdio commands.
// Secrets stay out of the unit file; Install warns about each one instead.
func TestInstallCarriesPathOverridesButNotSecrets(t *testing.T) {
	unixHost(t)

	cases := []struct {
		goos, golden, unitFile string
	}{
		{"darwin", "launchd-overrides.plist.golden", "home/Library/LaunchAgents/dev.wayfindr.daemon.plist"},
		{"linux", "systemd-overrides.service.golden", "xdg-config/systemd/user/wayfindr.service"},
	}

	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			m := newMachine(t)
			m.vars = map[string]string{ //nolint:gosec // fake secret values, to check they stay out of the unit
				"PATH":                  "/opt/homebrew/bin:/usr/bin:/bin",
				"WAYFINDR_CONFIG":       m.root + `/my "cfg"/wayfindr.toml`,
				"WAYFINDR_DATA_DIR":     m.root + "/data & 100%",
				"XDG_CONFIG_HOME":       m.root + "/xdg-config",
				"XDG_DATA_HOME":         m.root + "/xdg-data",
				"WAYFINDR_SECRET_KEY":   "secret-key-value",
				"WAYFINDR_CLIENT_TOKEN": "wf_client-token-value",
				"HOME":                  "/not/passed",
			}
			opts := m.options(tc.goos)
			opts.Binary = "/Users/Jane Doe/bin/wayfindr"

			res, err := service.Install(context.Background(), opts)

			if err != nil {
				t.Fatal(err)
			}

			unitFile := filepath.Join(m.root, filepath.FromSlash(tc.unitFile))

			if res.UnitFile != unitFile {
				t.Errorf("UnitFile = %q, want %q", res.UnitFile, unitFile)
			}

			if want := filepath.Join(m.root, "data & 100%", "wayfindr.log"); res.LogFile != want {
				t.Errorf("LogFile = %q, want %q", res.LogFile, want)
			}

			assertGolden(t, m, unitFile, tc.golden)

			for _, name := range []string{"WAYFINDR_SECRET_KEY", "WAYFINDR_CLIENT_TOKEN"} {
				if !containsAny(res.Warnings, name) {
					t.Errorf("Warnings = %q, want one naming %s", res.Warnings, name)
				}
			}

			for _, w := range res.Warnings {
				if strings.Contains(w, "secret-key-value") || strings.Contains(w, "wf_client-token-value") {
					t.Errorf("warning %q contains a secret value", w)
				}
			}
		})
	}
}

const (
	launchdPrint = "launchctl print gui/501/dev.wayfindr.daemon"
	plistFile    = "<root>/home/Library/LaunchAgents/dev.wayfindr.daemon.plist"
	systemdUnit  = "<root>/home/.config/systemd/user/wayfindr.service"
)

func TestInstallLoadsTheUnit(t *testing.T) {
	unixHost(t)

	notLoaded := errors.New("exit status 113: Could not find service")
	cases := []struct {
		name string
		goos string
		fail map[string]error
		want []string
	}{
		{
			name: "launchd first install",
			goos: "darwin",
			fail: map[string]error{launchdPrint: notLoaded},
			want: []string{
				launchdPrint,
				"launchctl enable gui/501/dev.wayfindr.daemon",
				"launchctl bootstrap gui/501 " + plistFile,
			},
		},
		{
			name: "launchd reinstall boots out the running agent first",
			goos: "darwin",
			want: []string{
				launchdPrint,
				"launchctl bootout gui/501/dev.wayfindr.daemon",
				"launchctl enable gui/501/dev.wayfindr.daemon",
				"launchctl bootstrap gui/501 " + plistFile,
			},
		},
		{
			name: "systemd install or reinstall",
			goos: "linux",
			want: []string{
				"systemctl --user daemon-reload",
				"systemctl --user enable wayfindr.service",
				"systemctl --user restart wayfindr.service",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t)
			m.fail = tc.fail

			res, err := service.Install(context.Background(), m.options(tc.goos))

			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, m.ran); diff != "" {
				t.Errorf("commands differ (-want +got):\n%s", diff)
			}

			// The log file exists before the service manager opens it, so
			// launchd appends to a 0600 file instead of creating a 0644 one.
			info, err := os.Stat(res.LogFile)

			if err != nil {
				t.Fatalf("log file: %v", err)
			}

			if got := info.Mode().Perm(); got != paths.SecretFileMode {
				t.Errorf("log file mode = %v, want %v", got, paths.SecretFileMode)
			}
		})
	}
}

func TestInstallReplacesTheUnitAndKeepsTheLog(t *testing.T) {
	unixHost(t)

	m := newMachine(t)
	first, err := service.Install(context.Background(), m.options("linux"))

	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(first.LogFile, []byte("earlier log\n"), paths.SecretFileMode); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(first.UnitFile, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := service.Install(context.Background(), m.options("linux")); err != nil {
		t.Fatal(err)
	}

	assertGolden(t, m, first.UnitFile, "systemd.service.golden")
	log, err := os.ReadFile(first.LogFile)

	if err != nil {
		t.Fatal(err)
	}

	if string(log) != "earlier log\n" {
		t.Errorf("log = %q, want it kept", log)
	}
}

func TestInstallFailsWhenTheServiceManagerRefuses(t *testing.T) {
	unixHost(t)

	m := newMachine(t)
	refused := errors.New("exit status 5: Bootstrap failed: 5: Input/output error")
	m.fail = map[string]error{
		launchdPrint: errors.New("exit status 113"),
		"launchctl bootstrap gui/501 " + plistFile: refused,
	}

	_, err := service.Install(context.Background(), m.options("darwin"))

	if !errors.Is(err, refused) {
		t.Errorf("err = %v, want it to wrap the launchctl error", err)
	}
}

func TestUninstallUnloadsAndRemovesTheUnit(t *testing.T) {
	unixHost(t)

	cases := []struct {
		name     string
		goos     string
		fail     map[string]error
		unitFile string
		want     []string
	}{
		{
			name:     "launchd loaded",
			goos:     "darwin",
			unitFile: plistFile,
			want:     []string{launchdPrint, "launchctl bootout gui/501/dev.wayfindr.daemon"},
		},
		{
			name:     "launchd not loaded",
			goos:     "darwin",
			fail:     map[string]error{launchdPrint: errors.New("exit status 113")},
			unitFile: plistFile,
			want:     []string{launchdPrint},
		},
		{
			name:     "systemd",
			goos:     "linux",
			unitFile: systemdUnit,
			want: []string{
				"systemctl --user disable --now wayfindr.service",
				"systemctl --user daemon-reload",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMachine(t)
			m.fail = map[string]error{launchdPrint: errors.New("exit status 113")}

			if _, err := service.Install(context.Background(), m.options(tc.goos)); err != nil {
				t.Fatal(err)
			}

			m.ran, m.fail = nil, tc.fail

			res, err := service.Uninstall(context.Background(), m.options(tc.goos))

			if err != nil {
				t.Fatal(err)
			}

			if diff := cmp.Diff(tc.want, m.ran); diff != "" {
				t.Errorf("commands differ (-want +got):\n%s", diff)
			}

			unitFile := strings.ReplaceAll(tc.unitFile, "<root>", m.root)

			if res.UnitFile != unitFile || !res.Removed {
				t.Errorf("result = %+v, want UnitFile %q removed", res, unitFile)
			}

			if _, err := os.Stat(unitFile); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("unit file still there: stat err = %v", err)
			}
		})
	}
}

func TestUninstallWithNoUnitDoesNothing(t *testing.T) {
	unixHost(t)

	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			m := newMachine(t)

			res, err := service.Uninstall(context.Background(), m.options(goos))

			if err != nil {
				t.Fatal(err)
			}

			if res.Removed {
				t.Error("Removed = true, want false")
			}

			if res.UnitFile == "" {
				t.Error("UnitFile is empty, want the path that was checked")
			}

			if len(m.ran) != 0 {
				t.Errorf("ran %q, want no commands", m.ran)
			}
		})
	}
}

func TestOtherOSIsNotSupported(t *testing.T) {
	m := newMachine(t)

	_, installErr := service.Install(context.Background(), m.options("windows"))
	_, uninstallErr := service.Uninstall(context.Background(), m.options("windows"))

	for _, err := range []error{installErr, uninstallErr} {
		if !errors.Is(err, service.ErrUnsupported) || !strings.Contains(err.Error(), "windows") {
			t.Errorf("err = %v, want ErrUnsupported naming windows", err)
		}
	}

	if len(m.ran) != 0 {
		t.Errorf("ran %q, want no commands", m.ran)
	}

	if entries, _ := os.ReadDir(m.root); len(entries) != 0 {
		t.Errorf("wrote %d entries under the temp root, want none", len(entries))
	}
}

func containsAny(ss []string, sub string) bool {
	for _, s := range ss {
		if strings.Contains(s, sub) {
			return true
		}
	}

	return false
}

// BeforeLoad runs once the unit is written and before the service manager
// runs, and not at all on an OS with no service manager, so what the CLI
// does there (stop the running daemon, wayfindr-p9vn) never happens for an
// install that can't go ahead.
func TestInstallRunsBeforeLoadJustBeforeLoading(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		m := newMachine(t)
		opts := m.options("windows")
		called := false
		opts.BeforeLoad = func(context.Context) error { called = true; return nil }

		if _, err := service.Install(context.Background(), opts); !errors.Is(err, service.ErrUnsupported) || called {
			t.Errorf("Install: %v, BeforeLoad called = %v; want ErrUnsupported and no call", err, called)
		}
	})

	t.Run("linux", func(t *testing.T) {
		unixHost(t)
		m := newMachine(t)
		opts := m.options("linux")
		var ranBefore int
		var unitThen error
		opts.BeforeLoad = func(context.Context) error {
			ranBefore = len(m.ran)
			_, unitThen = os.Stat(filepath.Join(m.root, "home", ".config", "systemd", "user", "wayfindr.service"))

			return nil
		}

		if _, err := service.Install(context.Background(), opts); err != nil {
			t.Fatal(err)
		}

		if ranBefore != 0 || unitThen != nil {
			t.Errorf("BeforeLoad ran after %d commands with the unit %v; want before any, with the unit written", ranBefore, unitThen)
		}
	})

	t.Run("its error stops the install", func(t *testing.T) {
		unixHost(t)
		m := newMachine(t)
		opts := m.options("linux")
		stop := errors.New("stop failed")
		opts.BeforeLoad = func(context.Context) error { return stop }

		if _, err := service.Install(context.Background(), opts); !errors.Is(err, stop) || len(m.ran) != 0 {
			t.Errorf("Install: %v after %v; want the hook's error and no command", err, m.ran)
		}
	})
}

// Restart reloads an installed unit with the same commands Install loads it
// with, so the service runs the binary that `wayfindr update` swapped in.
func TestRestartReloadsAnInstalledUnit(t *testing.T) {
	unixHost(t)

	cases := []struct {
		goos string
		want []string
	}{
		{
			goos: "darwin",
			want: []string{
				launchdPrint,
				"launchctl bootout gui/501/dev.wayfindr.daemon",
				"launchctl enable gui/501/dev.wayfindr.daemon",
				"launchctl bootstrap gui/501 " + plistFile,
			},
		},
		{
			goos: "linux",
			want: []string{
				"systemctl --user daemon-reload",
				"systemctl --user enable wayfindr.service",
				"systemctl --user restart wayfindr.service",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.goos, func(t *testing.T) {
			m := newMachine(t)

			if _, err := service.Install(context.Background(), m.options(tc.goos)); err != nil {
				t.Fatal(err)
			}

			m.ran = nil

			restarted, err := service.Restart(context.Background(), m.options(tc.goos))

			if err != nil {
				t.Fatal(err)
			}

			if !restarted {
				t.Error("restarted = false, want true")
			}

			if diff := cmp.Diff(tc.want, m.ran); diff != "" {
				t.Errorf("commands differ (-want +got):\n%s", diff)
			}
		})
	}
}

// With no unit installed, or on an OS with no service manager, there is no
// service to restart: Restart reports false and runs nothing.
func TestRestartWithNoUnitDoesNothing(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			if goos != "windows" {
				unixHost(t)
			}

			m := newMachine(t)

			restarted, err := service.Restart(context.Background(), m.options(goos))

			if err != nil || restarted {
				t.Errorf("Restart = %v, %v; want false, nil", restarted, err)
			}

			if len(m.ran) != 0 {
				t.Errorf("ran %q, want no commands", m.ran)
			}
		})
	}
}

func TestRestartFailsWhenTheServiceManagerRefuses(t *testing.T) {
	unixHost(t)

	m := newMachine(t)

	if _, err := service.Install(context.Background(), m.options("linux")); err != nil {
		t.Fatal(err)
	}

	refused := errors.New("exit status 1: Failed to restart wayfindr.service")
	m.fail = map[string]error{"systemctl --user restart wayfindr.service": refused}

	restarted, err := service.Restart(context.Background(), m.options("linux"))

	if !errors.Is(err, refused) || restarted {
		t.Errorf("Restart = %v, %v; want false and the runner's error", restarted, err)
	}
}

// Restart runs BeforeLoad just before it loads the unit, as Install does, so
// the CLI can stop a daemon holding the lock that the unit's new daemon
// would otherwise find running and exit on (wayfindr-p9vn). With no unit to
// restart it runs nothing, the hook included.
func TestRestartRunsBeforeLoadJustBeforeLoading(t *testing.T) {
	for _, goos := range []string{"darwin", "linux", "windows"} {
		t.Run(goos+" without a unit", func(t *testing.T) {
			if goos != "windows" {
				unixHost(t)
			}

			m := newMachine(t)
			opts := m.options(goos)
			called := false
			opts.BeforeLoad = func(context.Context) error { called = true; return nil }

			if restarted, err := service.Restart(context.Background(), opts); err != nil || restarted || called {
				t.Errorf("Restart = %v, %v, BeforeLoad called = %v; want false, nil, and no call", restarted, err, called)
			}
		})
	}

	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			unixHost(t)

			m := newMachine(t)

			if _, err := service.Install(context.Background(), m.options(goos)); err != nil {
				t.Fatal(err)
			}

			m.ran = nil
			opts := m.options(goos)
			ranBefore := -1
			opts.BeforeLoad = func(context.Context) error {
				ranBefore = len(m.ran)

				return nil
			}

			restarted, err := service.Restart(context.Background(), opts)

			if err != nil || !restarted {
				t.Fatalf("Restart = %v, %v; want true, nil", restarted, err)
			}

			if ranBefore != 0 || len(m.ran) == 0 {
				t.Errorf("BeforeLoad ran after %d commands, then %q ran; want it before any, then the load", ranBefore, m.ran)
			}
		})
	}

	t.Run("its error stops the restart", func(t *testing.T) {
		unixHost(t)

		m := newMachine(t)

		if _, err := service.Install(context.Background(), m.options("linux")); err != nil {
			t.Fatal(err)
		}

		m.ran = nil
		opts := m.options("linux")
		stop := errors.New("stop failed")
		opts.BeforeLoad = func(context.Context) error { return stop }

		if restarted, err := service.Restart(context.Background(), opts); !errors.Is(err, stop) || restarted || len(m.ran) != 0 {
			t.Errorf("Restart = %v, %v after %q; want false, the hook's error, and no command", restarted, err, m.ran)
		}
	})
}
