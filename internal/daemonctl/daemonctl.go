// Package daemonctl controls the daemon from outside it: start it in the
// background ([Ensure]), stop it ([Stop]), and read its log ([Tail]). The
// shim (`wayfindr stdio`) and the CLI's `start`, `stop`, and `logs`
// share it (design §7.9). It reaches the daemon only over HTTP and through
// the daemon lock, never by importing `daemon`.
package daemonctl

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemonlock"
	"github.com/atomicpages/wayfindr/internal/paths"
)

const (
	// startTimeout is how long Ensure waits for a daemon it started to
	// answer /healthz (design §7.9).
	startTimeout = 5 * time.Second
	// pollInterval is the pause between /healthz probes while it waits.
	pollInterval = 50 * time.Millisecond
	// probeTimeout bounds one /healthz probe.
	probeTimeout = time.Second
)

// Target is the daemon of one config and data directory, as seen from
// outside: an address to probe, the daemon lock to consult, and the log a
// daemon started in the background writes to.
type Target struct {
	// Listen is server.listen from the config file.
	Listen   string
	LockFile string
	LogFile  string
	// ConfigFile and DataDir are where the target's files were resolved.
	// A daemon Ensure starts gets them as WAYFINDR_CONFIG and
	// WAYFINDR_DATA_DIR, so it uses the same files as its starter whether
	// they came from flags, the environment, or the defaults. Empty means
	// the daemon resolves its own from the inherited environment.
	ConfigFile string
	DataDir    string
	Log        zerolog.Logger
}

// NewTarget returns the target for p. It reads only server.listen from the
// config file: a daemon keeps running its last good config when the file
// goes bad, and must still be reachable.
func NewTarget(p paths.Paths, log zerolog.Logger) (Target, error) {
	listen, err := config.ListenAddress(p.ConfigFile)

	if err != nil {
		return Target{}, err
	}

	return Target{
		Listen:     listen,
		LockFile:   p.LockFile(),
		LogFile:    p.LogFile(),
		ConfigFile: p.ConfigFile,
		DataDir:    p.DataDir,
		Log:        log,
	}, nil
}

// URL is the daemon's base URL, without a path.
func (t Target) URL() string { return "http://" + t.Listen }

// StartOptions shape the daemon Ensure starts, if it starts one.
type StartOptions struct {
	// Args are passed to `start --foreground` after it, such as --log-level.
	Args []string
	// LogFile takes the daemon's output; empty means Target.LogFile.
	LogFile string
}

// Running is the daemon Ensure found or started.
type Running struct {
	// Holder is the daemon as the lock names it: its pid is zero if the
	// lock could not be read.
	Holder daemonlock.Holder
	// Started is true when Ensure started it.
	Started bool
	// LogFile is where the daemon Ensure started writes its output: empty
	// when it started none, or when the log could not be opened and the
	// output is discarded.
	LogFile string
}

var (
	// ErrStarting is Verify's error when a daemon holds the daemon lock
	// with this server.listen but does not answer /healthz yet.
	ErrStarting = errors.New("the daemon is starting")
	// ErrNotOurs is wrapped by Verify's error when something answers
	// /healthz on the listen address but does not hold the daemon lock.
	ErrNotOurs = errors.New("does not hold the daemon lock")
)

// Verify reports whether this target's daemon runs: it answers /healthz on
// t.Listen and holds the daemon lock with that address. Only that daemon
// may be sent the client token. The lock is in the user's own data
// directory, so a process that answers without holding it is another
// program, or a daemon started with another data directory, and Verify
// refuses it with an error wrapping ErrNotOurs (wayfindr-zvjw).
//
// Otherwise it returns ErrNotRunning when nothing answers and the lock is
// free, ErrStarting when the lock is held with this address but nothing
// answers yet, an error naming a holder started with another server.listen
// (the config was edited and the daemon not restarted), or the lock's read
// error.
//
// /healthz is asked before the lock is read: a daemon takes the lock before
// it binds and lets go of it only after its server has closed, so a daemon
// that answered still holds the lock when it is read.
func (t Target) Verify(ctx context.Context) (daemonlock.Holder, error) {
	healthy := t.healthy(ctx)
	h, held, err := daemonlock.Probe(t.LockFile)

	if err != nil {
		return daemonlock.Holder{}, err
	}

	switch {
	case held && h.Listen != "" && h.Listen != t.Listen:
		return h, fmt.Errorf("daemon already running at %s (pid %d), but server.listen is now %s: restart the daemon to apply it", h.Listen, h.PID, t.Listen)
	case held && healthy:
		return h, nil
	case held:
		return h, ErrStarting
	case healthy:
		return daemonlock.Holder{}, fmt.Errorf("something answers on %s but %w %s: it is another program, or a daemon started with another data directory; stop it or change server.listen",
			t.Listen, ErrNotOurs, t.LockFile)
	default:
		return daemonlock.Holder{}, ErrNotRunning
	}
}

// DialContext dials addr, the daemon's address, only once Verify finds the
// daemon, so a client holding the client token checks it on every new
// connection, not just the first. The gateway is stateless: after the
// daemon stops or restarts, a client with a session simply dials again, and
// something else may hold the address by then (wayfindr-zvjw).
func (t Target) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if _, err := t.Verify(ctx); err != nil {
		return nil, err
	}

	var d net.Dialer

	return d.DialContext(ctx, network, addr)
}

// Transport is an HTTP transport for requests to the daemon that dials
// through DialContext. Anything that sends the daemon the client token or
// a secret uses it.
func (t Target) Transport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert // the standard library's own
	tr.DialContext = t.DialContext

	return tr
}

// Ensure returns once the target's daemon runs (see Verify), starting it
// first if nothing answers and no daemon holds the daemon lock. A daemon
// that holds it with another server.listen is one that was not restarted
// after the config changed: Ensure reports it rather than start a second
// daemon on the same store. One that holds it with this server.listen is
// starting, and Ensure waits for it. Something that answers without holding
// the lock is refused, and no daemon is started to contend for its address.
//
// Two callers may both find no daemon and both start one: the first to
// take the lock wins, the other exits with "daemon already running" (design
// §7.8), and both see the winner answer. So a started daemon that exits is
// an error only when no daemon answers and none holds the lock.
func Ensure(ctx context.Context, t Target, opts StartOptions) (Running, error) {
	holder, err := t.Verify(ctx)

	switch {
	case err == nil:
		return Running{Holder: holder}, nil
	case errors.Is(err, ErrStarting), errors.Is(err, ErrNotRunning):
	default:
		return Running{}, err
	}

	logFile := opts.LogFile

	if logFile == "" {
		logFile = t.LogFile
	}

	// exited is closed when the daemon Ensure starts exits; nil (never
	// ready) when it starts none.
	var exited <-chan struct{}
	running := Running{}

	if errors.Is(err, ErrStarting) {
		t.Log.Info().Str("listen", t.URL()).Msg("daemon lock held; waiting for the daemon to answer")
	} else {
		t.Log.Info().Str("listen", t.URL()).Msg("daemon not running; starting it")

		if exited, running.LogFile, err = start(ctx, t, logFile, opts.Args); err != nil {
			return Running{}, err
		}

		running.Started = true
	}

	deadline := time.NewTimer(startTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return Running{}, ctx.Err()
		case <-deadline.C:
			return Running{}, fmt.Errorf("the daemon did not answer %s/healthz within %s; see %s", t.URL(), startTimeout, logFile)
		case <-exited:
			exited = nil

			// It lost a start race to a daemon that holds the lock, or it
			// failed on its own: only the second is worth waiting out.
			// Something that took the address meanwhile is refused now.
			switch _, err := t.Verify(ctx); {
			case errors.Is(err, ErrNotRunning):
				return Running{}, fmt.Errorf("the daemon exited before it answered %s/healthz; see %s", t.URL(), logFile)
			case err != nil && !errors.Is(err, ErrStarting):
				return Running{}, err
			}
		case <-tick.C:
			switch h, err := t.Verify(ctx); {
			case err == nil:
				t.Log.Info().Msg("daemon started")

				running.Holder = h

				return running, nil
			case !errors.Is(err, ErrStarting) && !errors.Is(err, ErrNotRunning):
				return Running{}, err
			}
		}
	}
}

// healthy reports whether the daemon answers /healthz.
func (t Target) healthy(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL()+"/healthz", http.NoBody)

	if err != nil {
		return false
	}

	resp, err := http.DefaultClient.Do(req)

	if err != nil {
		return false
	}

	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// start spawns `wayfindr start --foreground` from this executable, detached from the
// caller (its own session or process group, no console) so it outlives it
// and the terminal's or client's signals. It inherits the environment
// except a scoped client token (see clienttoken.WithoutScoped), with the
// target's config file and data directory set over it (see
// Target.ConfigFile); its output is appended to logFile, or discarded if
// that can't be opened. It returns a channel closed when the daemon exits,
// and the log it writes to (empty if discarded).
func start(ctx context.Context, t Target, logFile string, args []string) (<-chan struct{}, string, error) {
	log := t.Log

	exe, err := os.Executable()

	if err != nil {
		return nil, "", fmt.Errorf("start daemon: %w", err)
	}

	// The data directory may not exist yet: the daemon would create it
	// (0700) when it takes the lock, after its first lines are lost.
	out, err := openLog(logFile)

	if err != nil {
		log.Warn().Err(err).Msg("daemon log file; discarding the daemon's output")
		logFile = ""

		if out, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0); err != nil {
			return nil, "", fmt.Errorf("start daemon: %w", err)
		}
	}

	// The child holds its own copy of the descriptor.
	defer func() { _ = out.Close() }()

	// The daemon outlives the request that started it, and the caller.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe, append([]string{"start", "--foreground"}, args...)...) //nolint:gosec // G204: this executable, its own start command
	cmd.Env = t.environ(clienttoken.WithoutScoped(os.Environ()))
	cmd.Stdout = out
	cmd.Stderr = out
	cmd.SysProcAttr = detached()

	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("start daemon: %w", err)
	}

	// Reap it if it exits while the caller runs, as the loser of a start
	// race does; otherwise it outlives the caller and is reparented.
	exited := make(chan struct{})

	go func() {
		_ = cmd.Wait()

		close(exited)
	}()

	return exited, logFile, nil
}

// environ is env with the target's config file and data directory set over
// it: exec uses the last value of a variable listed twice.
func (t Target) environ(env []string) []string {
	if t.ConfigFile != "" {
		env = append(env, paths.ConfigEnvVar+"="+t.ConfigFile)
	}

	if t.DataDir != "" {
		env = append(env, paths.DataDirEnvVar+"="+t.DataDir)
	}

	return env
}

// openLog opens logFile for appending (0600), creating it and its
// directory (0700) if missing.
func openLog(logFile string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logFile), 0o700); err != nil {
		return nil, err
	}

	return os.OpenFile(logFile, os.O_WRONLY|os.O_CREATE|os.O_APPEND, paths.SecretFileMode) //nolint:gosec // the data directory's log file, or the user's own --log-file
}
