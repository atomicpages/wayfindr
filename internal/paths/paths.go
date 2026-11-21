// Package paths resolves where Wayfindr keeps its config file and data
// directory, and the files derived from the data directory (design §3).
//
// The layout is XDG-style on every OS, macOS and Windows included, so one
// set of docs and dotfiles works everywhere. The package does no I/O beyond
// reading the injected environment; callers create directories and files.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
)

// Env is the slice of the process environment Resolve reads. It is injected
// so callers and tests control it without mutating process state.
type Env struct {
	// Getenv returns a variable's value, or "" when it is unset.
	Getenv func(key string) string
	// HomeDir returns the user's home directory.
	HomeDir func() (string, error)
}

// OS returns an Env backed by the real process environment.
func OS() Env {
	return Env{Getenv: os.Getenv, HomeDir: os.UserHomeDir}
}

// The variables that override the config file and the data directory. The
// CLI's --config and --data-dir flags set them for everything it resolves
// and starts.
const (
	ConfigEnvVar  = "WAYFINDR_CONFIG"
	DataDirEnvVar = "WAYFINDR_DATA_DIR"
)

// SecretFileMode is the permission mode for the client token and key files.
const SecretFileMode os.FileMode = 0o600

// Paths is the resolved location of the config file and data directory.
type Paths struct {
	ConfigFile string
	DataDir    string
}

// DBFile is the SQLite database, the default for storage.dsn.
func (p Paths) DBFile() string { return filepath.Join(p.DataDir, "wayfindr.db") }

// ClientTokenFile holds the client token. Write it with SecretFileMode.
func (p Paths) ClientTokenFile() string { return filepath.Join(p.DataDir, "client-token") }

// KeyFile holds the master key when no OS keychain is available. Write it
// with SecretFileMode.
func (p Paths) KeyFile() string { return filepath.Join(p.DataDir, "master.key") }

// LockFile is the daemon lock: the running daemon holds it, so a second
// daemon on the data directory refuses to start (internal/daemonlock).
func (p Paths) LockFile() string { return filepath.Join(p.DataDir, "daemon.lock") }

// LogFile takes the JSON log of a daemon that runs in the background: one
// started by `start` or the shim, or run as a service. Write it
// with SecretFileMode.
func (p Paths) LogFile() string { return filepath.Join(p.DataDir, "wayfindr.log") }

// TelemetryIDFile holds the install ID of the usage report (ADR 0020). It
// exists only once telemetry has run; deleting it gives the install a new
// ID.
func (p Paths) TelemetryIDFile() string { return filepath.Join(p.DataDir, "telemetry-id") }

// Resolve computes the config file and data directory from env. For each,
// the first match wins:
//
//   - config: WAYFINDR_CONFIG, then $XDG_CONFIG_HOME/wayfindr/wayfindr.toml,
//     then ~/.config/wayfindr/wayfindr.toml;
//   - data: WAYFINDR_DATA_DIR, then $XDG_DATA_HOME/wayfindr, then
//     ~/.local/share/wayfindr.
//
// WAYFINDR_* values are used as given. XDG values that are not absolute are
// ignored, as the XDG spec requires. The home directory is looked up only
// when a default is needed, so both overrides together work without one.
func Resolve(env Env) (Paths, error) {
	configFile, err := resolve(env, ConfigEnvVar, "XDG_CONFIG_HOME",
		[]string{".config"}, "wayfindr", "wayfindr.toml")

	if err != nil {
		return Paths{}, err
	}

	dataDir, err := resolve(env, DataDirEnvVar, "XDG_DATA_HOME",
		[]string{".local", "share"}, "wayfindr")

	if err != nil {
		return Paths{}, err
	}

	return Paths{ConfigFile: configFile, DataDir: dataDir}, nil
}

// resolve returns the override in overrideKey if set. Otherwise it joins
// rel onto the XDG base directory in xdgKey, or onto ~/homeRel… when that is
// unset or relative.
func resolve(env Env, overrideKey, xdgKey string, homeRel []string, rel ...string) (string, error) {
	if p := env.Getenv(overrideKey); p != "" {
		return p, nil
	}

	base := env.Getenv(xdgKey)

	if !filepath.IsAbs(base) {
		home, err := env.HomeDir()

		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}

		base = filepath.Join(append([]string{home}, homeRel...)...)
	}

	return filepath.Join(append([]string{base}, rel...)...), nil
}
