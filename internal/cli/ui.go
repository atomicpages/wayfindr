package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
)

// uiLoginTimeout bounds POST /admin/ui/login, which only mints a code.
const uiLoginTimeout = 10 * time.Second

func newUICmd(host Host) *cobra.Command {
	var noOpen bool

	cmd := &cobra.Command{
		Use:   "ui",
		Short: "Open the web console in the browser",
		Long: `Open the daemon's web console in the browser, signed in. The console shows
connectors, the call log, what search_tools returns for a query, and client
tokens. It can add connectors, sign them in and out, and create or revoke
tokens. It never edits or removes a connector.

The link carries a one-time code that works once, for one minute. The
browser stays signed in until it is idle for 12 hours or the daemon stops.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUI(cmd.Context(), host, cmd.OutOrStdout(), cmd.ErrOrStderr(), noOpen)
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "print the link instead of opening the browser")

	return cmd
}

func runUI(ctx context.Context, host Host, stdout, stderr io.Writer, noOpen bool) error {
	d, err := findDaemon(ctx, host)

	if err != nil {
		return err
	}

	lctx, cancel := context.WithTimeout(ctx, uiLoginTimeout)
	l, err := d.UILogin(lctx)
	cancel()

	if err != nil {
		return d.explain(err)
	}

	link := "http://" + d.listen + l.Path
	printLink := func(w io.Writer, lead string) error {
		_, err := fmt.Fprintf(w, "%s It works once, until %s:\n  %s\n", lead, formatTime(l.ExpiresAt), link)

		return err
	}

	if noOpen {
		return printLink(stdout, "Open this link to use the console.")
	}

	open := host.OpenURL

	if open == nil {
		open = openBrowser
	}

	if err := open(ctx, link); err != nil {
		return printLink(stderr, fmt.Sprintf("Could not open a browser (%v). Open this link yourself.", err))
	}

	_, err = fmt.Fprintf(stdout, "Opened the console at http://%s/ui/.\n", d.listen)

	return err
}
