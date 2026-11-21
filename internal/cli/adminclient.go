package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/clienttoken"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/daemonctl"
	"github.com/atomicpages/wayfindr/internal/paths"
)

// adminDaemon is the running daemon as the CLI finds it: the admin client
// at the config's listen address, presenting the client token as a bearer
// (design §7.7). Every command that talks to the admin API finds it with
// findDaemon, calls a Client method, and passes the error to explain.
type adminDaemon struct {
	listen string
	log    *zerolog.Logger // the command's, for explain
	*admin.Client
}

// findDaemon reads the config's listen address and the client token from
// host.
func findDaemon(ctx context.Context, host Host) (*adminDaemon, error) {
	p, err := paths.Resolve(host.Env)

	if err != nil {
		return nil, err
	}

	// Only the listen address: a config the daemon rejects must not stop
	// `wayfindr status` from showing why.
	listen, err := config.ListenAddress(p.ConfigFile)

	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		return nil, err
	}

	if err != nil {
		return nil, unreadableListen(ctx, host, p.ConfigFile, err)
	}

	token, err := clienttoken.Read(ctx, host.Env, p)

	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("client token: %w: run `wayfindr init` first", err)
	}

	if err != nil {
		return nil, fmt.Errorf("client token: %w", err)
	}

	// The token goes only to the daemon that holds this data directory's
	// lock, never to whatever answers on the address (wayfindr-zvjw).
	target := daemonctl.Target{Listen: listen, LockFile: p.LockFile(), Log: *zerolog.Ctx(ctx)}

	switch _, err := target.Verify(ctx); {
	case errors.Is(err, daemonctl.ErrNotRunning):
		return nil, notRunning(listen)
	case errors.Is(err, daemonctl.ErrStarting):
		return nil, fmt.Errorf("%w on %s yet: it is starting; try again in a moment", errDaemonNotRunning, listen)
	case err != nil:
		return nil, err
	}

	// A command that runs long dials again if the daemon restarts: each
	// dial is checked too.
	return &adminDaemon{listen: listen, log: zerolog.Ctx(ctx), Client: admin.NewClient("http://"+listen, token, &http.Client{Transport: target.Transport()})}, nil
}

// notRunning is the error for no daemon on listen, saying how to start one.
func notRunning(listen string) error {
	return fmt.Errorf("%w on %s: start it with `wayfindr start`, or run it as a service with `wayfindr service install`",
		errDaemonNotRunning, listen)
}

// unreadableListen explains a config file whose listen address can't be
// read (a TOML syntax error, or a malformed [server].listen). A daemon may
// still be running its last good config, so when one answers /healthz on
// the default address, the error says so.
func unreadableListen(ctx context.Context, host Host, path string, err error) error {
	advice := "Fix the config file " + path + "."

	if daemonAnswers(ctx, host, config.DefaultListen) {
		advice = fmt.Sprintf("The daemon is still running on %s with its last good config. %s", config.DefaultListen, advice)
	}

	return fmt.Errorf("%w\n%s", err, advice)
}

// healthzTimeout bounds the /healthz probe.
const healthzTimeout = 2 * time.Second

// daemonAnswers reports whether something answers GET /healthz with 200 on
// listen, dialing through host.Dial when set.
func daemonAnswers(ctx context.Context, host Host, listen string) bool {
	ctx, cancel := context.WithTimeout(ctx, healthzTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listen+"/healthz", nil)

	if err != nil {
		return false
	}

	client := &http.Client{}

	if host.Dial != nil {
		client.Transport = &http.Transport{DialContext: host.Dial}
	}

	resp, err := client.Do(req)

	if err != nil {
		return false
	}

	_ = resp.Body.Close()

	return resp.StatusCode == http.StatusOK
}

// errDaemonNotRunning means nothing accepted a connection on the listen
// address.
var errDaemonNotRunning = errors.New("daemon is not running")

// explain turns a Client error from a failed dial into one wrapping
// errDaemonNotRunning that says how to start the daemon; other errors pass
// through. An answer the daemon refused shows only its message, so the
// request and status go to the debug log.
func (d *adminDaemon) explain(err error) error {
	if ae := (*admin.APIError)(nil); errors.As(err, &ae) && d.log != nil {
		d.log.Debug().Str("method", ae.Method).Str("path", ae.Path).Int("status", ae.Status).Msg("admin API refused the request")
	}

	// A daemon gone by the time the client dials again is one the
	// transport's check finds not running.
	if op := (*net.OpError)(nil); (errors.As(err, &op) && op.Op == "dial") || errors.Is(err, daemonctl.ErrNotRunning) {
		return notRunning(d.listen)
	}

	return err
}
