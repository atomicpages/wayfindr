// Package setup implements `wayfindr init`: it prepares a host for its first
// daemon run (design §3, §7.9). internal/cli parses flags and calls Init;
// the work lives here so the CLI stays free of domain logic.
//
// Init is idempotent and never overwrites: every file it would create is
// left alone when it already exists, the master key is reused when any
// source holds one, and the local user is reused when the store has one.
package setup

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clientconfig"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/paths"
	"github.com/atomicpages/wayfindr/internal/secrets"
	"github.com/atomicpages/wayfindr/internal/store/drivers"
	"github.com/atomicpages/wayfindr/internal/telemetry"
)

// EnvClientToken holds the client token value. When set it takes precedence
// over <data>/client-token (design §3); Init still writes the file only when
// it is absent.
const EnvClientToken = clienttoken.EnvVar

// probeTimeout bounds the check for a running daemon. A daemon on loopback
// accepts well within it; a closed port is refused at once.
const probeTimeout = 500 * time.Millisecond

// ErrDaemonRunning means something accepts connections on the configured
// listen address. Init refuses to run then, because the daemon is the
// store's only writer (design §9).
var ErrDaemonRunning = errors.New("a daemon is listening")

// ErrUnsupportedStore means the config names a store driver Wayfindr does
// not know. It is drivers.ErrUnsupported, so either matches with errors.Is.
var ErrUnsupportedStore = drivers.ErrUnsupported

// Dialer connects to addr on network, as net.Dialer.DialContext does. Init
// uses one to detect a running daemon; tests substitute a fake.
type Dialer func(ctx context.Context, network, addr string) (net.Conn, error)

// Options is what Init reads from the host. Tests substitute fakes.
type Options struct {
	// Env is the process environment and home directory.
	Env paths.Env
	// Keychain is the OS keychain. Nil means none is available, so a new
	// master key goes to the key file.
	Keychain secrets.Keychain
	// Dial connects to the listen address to detect a running daemon. Nil
	// means a plain TCP dial.
	Dial Dialer
	// AskTelemetry asks whether to turn the usage report on and reports the
	// answer. Init calls it only when it is about to write a new config and
	// DO_NOT_TRACK is not set. Nil means no.
	AskTelemetry func() bool
	Logger       zerolog.Logger
}

// Result says what Init found or created. It holds no secret values.
type Result struct {
	Paths paths.Paths
	// Listen is the daemon's configured host:port.
	Listen string
	// ConfigCreated is true when Init wrote the default config.
	ConfigCreated bool
	// TelemetryEnabled is true when the config turns the usage report on
	// and DO_NOT_TRACK does not override it, whether Init just wrote it
	// (the user said yes) or found it.
	TelemetryEnabled bool
	// KeySource is where the master key lives.
	KeySource secrets.Source
	// UserID is the local user's id.
	UserID string
	// TokenCreated is true when Init wrote <data>/client-token.
	TokenCreated bool
	// TokenFromEnv is true when WAYFINDR_CLIENT_TOKEN is set, so it, not
	// the token file, is the client token.
	TokenFromEnv bool
	// Importable are the clients with a user-level MCP config, which
	// `wayfindr connector import` can read.
	Importable []clientconfig.Client
}

// Init writes the default config if there is none, refuses to continue if a
// daemon is listening, runs the store migrations, creates the local user and
// the master key, and writes the client token file. Running it again changes
// nothing.
func Init(ctx context.Context, opts Options) (Result, error) {
	p, err := paths.Resolve(opts.Env)

	if err != nil {
		return Result{}, err
	}

	res := Result{Paths: p}

	cfgData, err := os.ReadFile(p.ConfigFile)

	switch {
	case errors.Is(err, fs.ErrNotExist):
		cfgData, res.ConfigCreated = []byte(DefaultConfig), true
	case err != nil:
		return Result{}, fmt.Errorf("read config: %w", err)
	}

	cfg, err := config.Parse(cfgData, config.Options{DataDir: p.DataDir, HomeDir: opts.Env.HomeDir})

	if err != nil {
		return Result{}, fmt.Errorf("config %s: %w", p.ConfigFile, err)
	}

	res.Listen = cfg.Server.Listen
	res.TelemetryEnabled = cfg.Telemetry.Enabled && !telemetry.DoNotTrack(opts.Env.Getenv(telemetry.DoNotTrackEnv))

	// Check before writing anything, so a refused run leaves no trace.
	if err := probeDaemon(ctx, opts.Dial, cfg.Server.Listen); err != nil {
		return Result{}, err
	}

	if res.ConfigCreated {
		// Ask only about a config Init is writing: an existing one already
		// holds the user's choice. DO_NOT_TRACK keeps the report off whatever
		// the config says, so there is nothing to ask.
		if opts.AskTelemetry != nil && !telemetry.DoNotTrack(opts.Env.Getenv(telemetry.DoNotTrackEnv)) && opts.AskTelemetry() {
			cfgData, res.TelemetryEnabled = []byte(defaultConfigWithTelemetry()), true
		}

		if err := createFile(p.ConfigFile, cfgData, 0o600); err != nil {
			return Result{}, fmt.Errorf("write default config: %w", err)
		}

		opts.Logger.Info().Str("path", p.ConfigFile).Msg("default config written")
	}

	res.UserID, res.KeySource, err = initStore(ctx, opts, p, cfg.Storage)

	if err != nil {
		return Result{}, err
	}

	res.TokenFromEnv = opts.Env.Getenv(EnvClientToken) != ""
	res.TokenCreated, err = ensureClientToken(p.ClientTokenFile())

	if err != nil {
		return Result{}, err
	}

	if res.TokenCreated {
		opts.Logger.Info().Str("path", p.ClientTokenFile()).Msg("client token written")
	}

	// Without a home directory there is nothing to suggest importing.
	if home, err := opts.Env.HomeDir(); err == nil {
		res.Importable = clientconfig.Detect(clientconfig.Dirs{Home: home, GOOS: runtime.GOOS, Getenv: opts.Env.Getenv})
	}

	return res, nil
}

// probeDaemon returns ErrDaemonRunning when listen accepts a TCP connection.
func probeDaemon(ctx context.Context, dial Dialer, listen string) error {
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}

	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	conn, err := dial(ctx, "tcp", listen)

	if err != nil {
		return nil // a failed connect means no daemon is listening
	}

	_ = conn.Close()

	return fmt.Errorf("%w on %s: stop it before running init", ErrDaemonRunning, listen)
}

// initStore opens the configured store (running migrations), ensures the
// local user, and loads or creates the master key.
func initStore(ctx context.Context, opts Options, p paths.Paths, storage config.Storage) (userID string, source secrets.Source, err error) {
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return "", "", fmt.Errorf("create data directory: %w", err)
	}

	if storage.Driver == config.DriverSQLite {
		// The sqlite DSN is a file path; its directory must exist first.
		if err := os.MkdirAll(filepath.Dir(storage.DSN), 0o700); err != nil { //nolint:gosec // dsn is from the user's own config file
			return "", "", fmt.Errorf("create store directory: %w", err)
		}
	}

	s, err := drivers.Open(ctx, storage.Driver, storage.DSN)

	if err != nil {
		return "", "", err
	}

	defer func() {
		if cerr := s.Close(); err == nil && cerr != nil {
			err = cerr
		}
	}()

	userID, err = s.EnsureUser(ctx)

	if err != nil {
		return "", "", err
	}

	sealed, err := s.HasSealedData(ctx)

	if err != nil {
		return "", "", err
	}

	_, source, err = secrets.LoadOrCreate(ctx, secrets.KeySources{
		Getenv:   opts.Env.Getenv,
		Keychain: opts.Keychain,
		KeyFile:  p.KeyFile(),
		Logger:   opts.Logger,
	}, sealed)

	if err != nil {
		return "", "", fmt.Errorf("master key: %w", err)
	}

	return userID, source, nil
}

// ensureClientToken writes a new client token to path unless the file
// exists. It reports whether it wrote one.
func ensureClientToken(path string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("check client token: %w", err)
	}

	token, err := clienttoken.GenerateDefault()

	if err != nil {
		return false, err
	}

	if err := createFile(path, []byte(token), paths.SecretFileMode); err != nil {
		return false, fmt.Errorf("write client token: %w", err)
	}

	return true, nil
}

// createFile writes data to a new file at path with perm, creating parent
// directories. It fails if path exists, so it never overwrites.
func createFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", path, err)
	}

	return newfile.Create(path, data, perm, path)
}
