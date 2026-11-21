package cli

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
)

const (
	// authStartTimeout bounds POST /admin/auth/{connector}: discovery and,
	// for a new connector, client registration.
	authStartTimeout = 60 * time.Second
	// authPollWait is the long-poll's ?wait=.
	authPollWait = 30 * time.Second
	// authGrace is how long past the flow's expiry the command keeps
	// polling: the daemon reports the expiry itself, and a done flow waits
	// for the connector's reconnect.
	authGrace = 90 * time.Second
	// revokeTimeout bounds DELETE /admin/credentials/{connector}, which
	// waits for the connector's reconnect.
	revokeTimeout = 90 * time.Second
)

// newAuthCmd is the `auth` group. Its old spellings still work, hidden:
// `auth <connector>` is `auth login <connector>`, and `auth revoke` is
// `auth logout`. So, unlike the other groups, an argument that names no
// subcommand is a connector to log in to, not an unknown command.
func newAuthCmd(host Host) *cobra.Command {
	var flags authFlags

	cmd := &cobra.Command{
		Use:   "auth",
		Short: "Authorize an OAuth connector, or delete its stored credential",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}

			return flags.authorize(cmd, host, args[0])
		},
	}
	flags.add(cmd, true)

	cmd.AddCommand(newAuthLoginCmd(host), newAuthLogoutCmd(host, "logout", false), newAuthLogoutCmd(host, "revoke", true))

	return cmd
}

func newAuthLoginCmd(host Host) *cobra.Command {
	var flags authFlags

	cmd := &cobra.Command{
		Use:   "login <connector>",
		Short: "Authorize an OAuth connector in the browser or with a device code",
		Long: `Start the connector's OAuth authorization: print its URL, open it in the
browser, and wait until the browser comes back to the daemon, the token is
stored, and the connector has reconnected.

With --no-open the URL is only printed, for a machine with no browser (over
SSH, for example): open it in a browser that can reach the daemon's
callback, then the command finishes as usual.

With --device, or for a connector with grant = "device_code", the
authorization uses a device code instead (RFC 8628): the command prints a
code to enter at the provider's address, on this or any other device, and
the daemon finishes once you have allowed access. No browser needs to
reach the daemon. The provider must support it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return flags.authorize(cmd, host, args[0])
		},
	}
	flags.add(cmd, false)

	return cmd
}

// authFlags are the flags of `auth login`, which the hidden bare
// `auth <connector>` takes too.
type authFlags struct {
	noOpen, device bool
}

// add registers the flags on cmd, hidden on the bare `auth`, whose help is
// the group's.
func (f *authFlags) add(cmd *cobra.Command, hidden bool) {
	cmd.Flags().BoolVar(&f.noOpen, "no-open", false, "print the URL instead of opening the browser")
	cmd.Flags().BoolVar(&f.device, "device", false, "sign in with a code entered on any device instead of a browser redirect")

	if hidden {
		_ = cmd.Flags().MarkHidden("no-open")
		_ = cmd.Flags().MarkHidden("device")
	}
}

func (f *authFlags) authorize(cmd *cobra.Command, host Host, connector string) error {
	return authorize(cmd.Context(), host, cmd.OutOrStdout(), cmd.ErrOrStderr(), connector,
		authOptions{open: !f.noOpen, device: f.device})
}

// newAuthLogoutCmd is `auth logout`, or, hidden, its old name `auth revoke`.
func newAuthLogoutCmd(host Host, name string, hidden bool) *cobra.Command {
	return &cobra.Command{
		Use:    name + " <connector>",
		Short:  "Delete a connector's stored OAuth credential",
		Hidden: hidden,
		Long: `Delete the connector's stored OAuth token and the client Wayfindr registered
for it, and cancel its authorizations in progress. The connector then needs
auth login again. This also removes the orphaned credential of a connector no
longer in the config. Nothing is sent to the authorization server.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRevoke(cmd.Context(), host, cmd.OutOrStdout(), args[0])
		},
	}
}

func runRevoke(ctx context.Context, host Host, stdout io.Writer, connector string) error {
	d, err := findDaemon(ctx, host)

	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()
	r, err := d.Revoke(ctx, connector)

	if notFound(err) {
		_, err := fmt.Fprintf(stdout, "%s has no stored credential; nothing to revoke.\n", connector)

		return err
	}

	if err != nil {
		return d.explain(err)
	}

	var b bytes.Buffer

	if r.Result == nil {
		fmt.Fprintf(&b, "Revoked the credential of %s, which is no longer configured.\n", connector)
	} else {
		fmt.Fprintf(&b, "Revoked the credential of %s.\n", connector)
		renderConnector(&b, *r.Result)
	}

	_, err = stdout.Write(b.Bytes())

	return err
}

// runAuth runs the auth flow and opens its URL in the browser, as
// "connector add" does after adding an OAuth connector.
func runAuth(ctx context.Context, host Host, stdout, stderr io.Writer, connector string) error {
	return authorize(ctx, host, stdout, stderr, connector, authOptions{open: true})
}

// authOptions are how `wayfindr auth login` runs a flow.
type authOptions struct {
	// open opens the URL in the browser.
	open bool
	// device asks for the device code flow.
	device bool
}

// authorize starts the connector's auth flow, prints its URL, or its user
// code and where to enter it, opens that in the browser when opts.open is
// set, and waits for the flow to finish.
func authorize(ctx context.Context, host Host, stdout, stderr io.Writer, connector string, opts authOptions) error {
	d, err := findDaemon(ctx, host)

	if err != nil {
		return err
	}

	sctx, cancel := context.WithTimeout(ctx, authStartTimeout)
	f, err := d.StartAuth(sctx, connector, admin.AuthStart{Device: opts.device})
	cancel()

	if err != nil {
		return d.explain(err)
	}

	again := "wayfindr auth login " + connector

	if opts.device {
		again += " --device"
	}

	open := f.AuthorizeURL

	if f.Device() {
		open = cmp.Or(f.VerificationURIComplete, f.VerificationURI)
		_, err = fmt.Fprintf(stdout, "Enter code %s at %s to authorize %s.\n"+
			"Waiting for you to allow access (until %s). If the page shows an error, fix it and run `%s`; %s stays configured.\n",
			f.UserCode, f.VerificationURI, connector, formatTime(f.ExpiresAt), again, connector)
	} else {
		_, err = fmt.Fprintf(stdout, "Open this URL to authorize %s:\n  %s\n"+
			"Waiting for the browser (until %s). If the page shows an error, fix it and run `%s`; %s stays configured.\n",
			connector, f.AuthorizeURL, formatTime(f.ExpiresAt), again, connector)
	}

	if err != nil {
		return err
	}

	if opts.open {
		openAuthorizeURL(ctx, host, stderr, open)
	}

	// Ctrl-C while waiting says how to retry instead of just ending.
	ictx, stop := signal.NotifyContext(ctx, os.Interrupt)
	defer stop()
	wctx, cancel := context.WithDeadline(ictx, f.ExpiresAt.Add(authGrace))
	defer cancel()
	notAuthorized := fmt.Sprintf("%s was not authorized. Fix the cause, then run `%s`.\n", connector, again)

	for {
		s, err := d.WaitAuth(wctx, f.FlowID, authPollWait)

		if errors.Is(err, context.DeadlineExceeded) && ictx.Err() == nil {
			_, _ = fmt.Fprint(stdout, notAuthorized)

			return fmt.Errorf("the authorization of %s did not finish in time", connector)
		}

		if err != nil && ctx.Err() != nil {
			// The caller gave up, not the user: no Ctrl-C hint.
			return err
		}

		if err != nil && ictx.Err() != nil {
			// The prompt line may hold ^C; start a new one.
			_, _ = fmt.Fprint(stdout, "\n"+notAuthorized)

			return fmt.Errorf("the authorization of %s was interrupted", connector)
		}

		if err != nil {
			return d.explain(err)
		}

		var b bytes.Buffer

		switch s.State {
		case admin.FlowPending:
			continue
		case admin.FlowDone:
			writeAuthorized(&b, connector, s.Result)
			_, err := stdout.Write(b.Bytes())

			return err
		case admin.FlowFailed, admin.FlowExpired:
			b.WriteString(notAuthorized)

			if a := s.Advice; a != nil {
				renderConnector(&b, admin.Connector{Name: connector, Advice: a})
			}

			if _, err := stdout.Write(b.Bytes()); err != nil {
				return err
			}

			return fmt.Errorf("the authorization of %s %s", connector, s.State)
		}

		return fmt.Errorf("the daemon reported the authorization of %s as %q", connector, s.State)
	}
}

// openAuthorizeURL opens the printed authorize URL in the browser. A
// failure only says so: the URL is printed, so the user can open it by hand.
func openAuthorizeURL(ctx context.Context, host Host, stderr io.Writer, u string) {
	open := host.OpenURL

	if open == nil {
		open = openBrowser
	}

	if err := open(ctx, u); err != nil {
		_, _ = fmt.Fprintf(stderr, "Could not open a browser (%v); open the URL above yourself.\n", err)
	}
}

// writeAuthorized confirms the authorization and says how the connector
// came out of its reconnect: ready with its tools, or its advice.
func writeAuthorized(b *bytes.Buffer, connector string, c *admin.Connector) {
	fmt.Fprintf(b, "Authorized %s.\n", connector)

	if c != nil {
		renderSettled(b, *c)
	}
}

// openBrowser opens u in the user's browser with the platform's opener
// (design §7.9). It refuses anything but an http(s) URL, so a daemon can
// never make it run a file or another scheme's handler.
func openBrowser(ctx context.Context, u string) error {
	if p, err := url.Parse(u); err != nil || (p.Scheme != "http" && p.Scheme != "https") {
		return errors.New("refusing to open the URL: it is not an http(s) URL")
	}

	// u is checked above to be an http(s) URL; it is one argument, never
	// a shell string.
	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "darwin":
		cmd = exec.CommandContext(ctx, "open", u) //nolint:gosec // G204: see above
	case "windows":
		cmd = exec.CommandContext(ctx, "rundll32", "url.dll,FileProtocolHandler", u) //nolint:gosec // G204: see above
	default:
		cmd = exec.CommandContext(ctx, "xdg-open", u) //nolint:gosec // G204: see above
	}

	if err := cmd.Start(); err != nil {
		return err
	}

	// Some openers wait for the browser; the command doesn't.
	go func() { _ = cmd.Wait() }()

	return nil
}
