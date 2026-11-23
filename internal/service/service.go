// Package service installs and removes the user-level unit that runs the
// daemon at login (design §7.9): a launchd agent on macOS, a systemd user
// unit on Linux. The unit runs `wayfindr start --foreground --log-file <data>/wayfindr.log`
// and restarts it when it fails.
//
// Everything the package reads from the machine (the OS, the environment,
// the home directory, the binary, and the launchctl or systemctl runner) is
// injected through Options, so tests never load a real service.
package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/atomicpages/wayfindr/internal/paths"
)

const (
	// Label is the launchd label of the agent, and the plist's base name.
	Label = "dev.wayfindr.daemon"
	// UnitName is the systemd user unit.
	UnitName = "wayfindr.service"
	// unitFileMode lets launchd load the plist: it refuses one that is
	// group- or world-writable.
	unitFileMode os.FileMode = 0o644
)

// Runner runs an external command (launchctl or systemctl) and returns an
// error, carrying its output, if it fails.
type Runner func(ctx context.Context, name string, args ...string) error

// Options is the machine Install and Uninstall act on.
type Options struct {
	// GOOS picks the service manager: "darwin" or "linux".
	GOOS string
	// UID is the user's id, for the launchd domain gui/<uid>.
	UID int
	// Binary is the absolute path of the wayfindr executable the unit runs,
	// symlinks resolved (see Executable).
	Binary string
	// Env is the installer's environment and home directory.
	Env paths.Env
	// Run runs launchctl or systemctl.
	Run Runner
	// BeforeLoad, if set, runs just before Install or Restart loads the
	// unit: once Install has written it, or once Restart has found it
	// installed. So nothing it does happens on an OS with no service manager,
	// when the unit can't be written, or when there is no unit to restart.
	// The CLI stops a running daemon there (wayfindr-p9vn), so the one the
	// unit starts takes the lock. An error stops the install or restart.
	BeforeLoad func(ctx context.Context) error
}

// InstallResult says what Install wrote.
type InstallResult struct {
	// UnitFile is the plist or unit file.
	UnitFile string
	// LogFile is where the daemon logs.
	LogFile string
	// Warnings name environment variables set in the installer's shell that
	// the service will not see.
	Warnings []string
}

// UninstallResult says what Uninstall removed.
type UninstallResult struct {
	// UnitFile is the plist or unit file Uninstall looked for.
	UnitFile string
	// Removed is false when there was no unit to remove.
	Removed bool
}

// ErrUnsupported is returned on an OS with neither launchd nor systemd.
var ErrUnsupported = errors.New("service: not supported")

// serviceManager is launchd or systemd.
type serviceManager interface {
	// unitFile is where the unit lives for this user.
	unitFile(env paths.Env) (string, error)
	// render is the unit file's content.
	render(u unit) string
	// load starts the unit just written, replacing a running one.
	load(ctx context.Context, unitFile string) error
	// remove stops the unit and deletes unitFile.
	remove(ctx context.Context, unitFile string) error
}

func manager(opts Options) (serviceManager, error) {
	switch opts.GOOS {
	case "darwin":
		return launchd{uid: opts.UID, run: opts.Run}, nil
	case "linux":
		return systemd{run: opts.Run}, nil
	default:
		return nil, fmt.Errorf("%w on %s: run `wayfindr start --foreground` under your own supervisor", ErrUnsupported, opts.GOOS)
	}
}

// Install writes the unit, replacing any previous one, and (re)loads it so
// the daemon runs now and at every login. It is safe to run again, for
// example after moving the binary or changing WAYFINDR_* overrides.
func Install(ctx context.Context, opts Options) (InstallResult, error) {
	m, err := manager(opts)

	if err != nil {
		return InstallResult{}, err
	}

	p, err := paths.Resolve(opts.Env)

	if err != nil {
		return InstallResult{}, err
	}

	unitFile, err := m.unitFile(opts.Env)

	if err != nil {
		return InstallResult{}, err
	}

	u := unit{
		binary:  opts.Binary,
		logFile: p.LogFile(),
		env:     passedEnv(opts.Env),
	}

	if err := createLogFile(p.DataDir, u.logFile); err != nil {
		return InstallResult{}, err
	}

	if err := writeUnit(unitFile, m.render(u)); err != nil {
		return InstallResult{}, err
	}

	if opts.BeforeLoad != nil {
		if err := opts.BeforeLoad(ctx); err != nil {
			return InstallResult{}, err
		}
	}

	if err := m.load(ctx, unitFile); err != nil {
		return InstallResult{}, err
	}

	return InstallResult{UnitFile: unitFile, LogFile: u.logFile, Warnings: secretWarnings(opts.Env)}, nil
}

// Uninstall stops the unit and removes it. With no unit file it does
// nothing and reports Removed false.
func Uninstall(ctx context.Context, opts Options) (UninstallResult, error) {
	m, err := manager(opts)

	if err != nil {
		return UninstallResult{}, err
	}

	unitFile, err := m.unitFile(opts.Env)

	if err != nil {
		return UninstallResult{}, err
	}

	if _, err := os.Stat(unitFile); errors.Is(err, os.ErrNotExist) {
		return UninstallResult{UnitFile: unitFile}, nil
	} else if err != nil {
		return UninstallResult{}, fmt.Errorf("check %s: %w", unitFile, err)
	}

	if err := m.remove(ctx, unitFile); err != nil {
		return UninstallResult{}, err
	}

	return UninstallResult{UnitFile: unitFile, Removed: true}, nil
}

// Restart reloads an installed unit so the service runs the binary its unit
// names afresh, as `wayfindr update` needs after replacing that binary in
// place. It loads the unit rather than stopping the daemon, because neither
// manager restarts a clean exit (launchd's KeepAlive has SuccessfulExit
// false, systemd has Restart=on-failure); loading replaces a running unit
// and starts the new binary. It runs opts.BeforeLoad just before loading, as
// Install does: a daemon that holds the lock (one started by hand, say)
// would otherwise make the unit's new daemon find it running and exit
// cleanly, which neither manager restarts. It reports false, and runs
// nothing, when no unit file is installed or the OS has no supported
// service manager (Windows): there is no service to restart.
func Restart(ctx context.Context, opts Options) (bool, error) {
	m, err := manager(opts)

	if errors.Is(err, ErrUnsupported) {
		return false, nil
	} else if err != nil {
		return false, err
	}

	unitFile, err := m.unitFile(opts.Env)

	if err != nil {
		return false, err
	}

	if _, err := os.Stat(unitFile); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("check %s: %w", unitFile, err)
	}

	if opts.BeforeLoad != nil {
		if err := opts.BeforeLoad(ctx); err != nil {
			return false, err
		}
	}

	if err := m.load(ctx, unitFile); err != nil {
		return false, err
	}

	return true, nil
}

// unit is what the unit file runs.
type unit struct {
	binary  string
	logFile string
	env     []envVar
}

// args is the daemon's command line.
func (u unit) args() []string {
	return []string{u.binary, "start", "--foreground", "--log-file", u.logFile}
}

// envVar is one variable the unit sets.
type envVar struct{ name, value string }

// passedVars are the installer's variables the unit sets for the daemon, in
// the order the unit lists them. launchd and systemd start the daemon with
// an environment of their own, so without these it would resolve a
// different config file and data directory (design §3) and miss commands
// that stdio connectors run from the user's PATH. DO_NOT_TRACK goes too, so
// a user who set it keeps the usage report off under the service (ADR 0020).
func passedVars() []string {
	return []string{"PATH", "WAYFINDR_CONFIG", "WAYFINDR_DATA_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "DO_NOT_TRACK"}
}

// secretVars hold secret values (design §3, §9). They are never written to
// a unit file: the daemon falls back to the keychain or master.key, and to
// the client-token file.
func secretVars() []string {
	return []string{"WAYFINDR_SECRET_KEY", "WAYFINDR_CLIENT_TOKEN"}
}

func passedEnv(env paths.Env) []envVar {
	var vars []envVar

	for _, name := range passedVars() {
		if v := env.Getenv(name); v != "" {
			vars = append(vars, envVar{name, v})
		}
	}

	return vars
}

func secretWarnings(env paths.Env) []string {
	var warnings []string

	for _, name := range secretVars() {
		if env.Getenv(name) != "" {
			warnings = append(warnings, name+" is set in this shell but is not written to the unit, because it holds a secret; "+
				"the service will not see it and uses the keychain or the files in the data directory instead")
		}
	}

	return warnings
}

// createLogFile makes sure the log file exists with SecretFileMode before
// the service manager opens it: launchd would otherwise create it 0644 for
// the plist's StandardOutPath. An existing log is kept as it is.
func createLogFile(dataDir, logFile string) error {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	f, err := os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, paths.SecretFileMode) //nolint:gosec // the path is the user's own data dir

	if err != nil {
		return fmt.Errorf("create log file: %w", err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("create log file: %w", err)
	}

	return nil
}

func writeUnit(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // launchd and systemd unit dirs are conventionally 0755
		return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, []byte(content), unitFileMode); err != nil { //nolint:gosec // unit files hold no secrets; launchd needs them readable
		return fmt.Errorf("write %s: %w", path, err)
	}

	return nil
}

func removeUnit(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}

	return nil
}

// Executable returns the running binary's path with symlinks resolved, so
// the unit names the real file rather than a link that may be repointed.
func Executable() (string, error) {
	exe, err := os.Executable()

	if err != nil {
		return "", fmt.Errorf("find the wayfindr executable: %w", err)
	}

	resolved, err := filepath.EvalSymlinks(exe)

	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", exe, err)
	}

	return resolved, nil
}

// ExecRunner runs commands with os/exec. The error carries the command's
// combined output, which is where launchctl and systemctl explain failures.
func ExecRunner(ctx context.Context, name string, args ...string) error {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // name is launchctl or systemctl; args are built here

	if err != nil {
		cmdline := strings.Join(append([]string{name}, args...), " ")

		if msg := strings.TrimSpace(string(out)); msg != "" {
			return fmt.Errorf("%s: %w: %s", cmdline, err, msg)
		}

		return fmt.Errorf("%s: %w", cmdline, err)
	}

	return nil
}
