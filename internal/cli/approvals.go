package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

func newApprovalsCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List the calls waiting for your approval, and approve or deny them",
		Long: `A remote client token or OAuth grant can require your approval for some of
its calls (` + "`wayfindr token create --approval`" + `). Such a call is refused until you
approve it here or in the console, and the agent is told to wait and call
again.

With no subcommand this lists the queue: one row per call, then its
arguments, in full, exactly as the gateway holds them. Read the arguments
before you approve: an approval covers a call with exactly those arguments,
once. The agent's own description of the call is not shown.

Pending approvals last 24 hours, and an approved one 1 hour. Nothing a
client sends can approve or deny; only the admin API can, which a remote
client cannot reach.`,
		Example: `  wayfindr approvals
  wayfindr approvals approve a3f2
  wayfindr approvals approve a3f2 --yes
  wayfindr approvals deny a3f2`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runApprovalsList(cmd, host) },
	}
	cmd.AddCommand(newApprovalsListCmd(host), newApprovalsApproveCmd(host), newApprovalsDenyCmd(host))
	supportsJSON(cmd)

	return cmd
}

func newApprovalsListCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the calls waiting for your approval (the same as `wayfindr approvals`)",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runApprovalsList(cmd, host) },
	}
	supportsJSON(cmd)

	return cmd
}

// newApprovalsApproveCmd is `approvals approve <id>`. It shows the call
// before it approves it, and asks, or needs --yes: an agent can tell the
// owner to run it, and an approval covers whatever call the id names.
func newApprovalsApproveCmd(host Host) *cobra.Command {
	var yes bool

	cmd := &cobra.Command{
		Use:   "approve <id>",
		Short: "Approve a call, once, with exactly the arguments shown",
		Long: `Approve a call, once, with exactly the arguments shown.

The command first prints the call: its client, tool, class, state, and the
size of its arguments, then the arguments in full, as ` + "`wayfindr approvals`" + `
lists them. On a terminal it then asks before it approves, and only an
answer typed after the question counts: what was typed or pasted before it
is discarded. Elsewhere it approves only with --yes. Read the call before you approve it, even when
an agent asked you to run this command: the approval covers the call shown,
whatever the agent said it was.

The id is in the ` + "`wayfindr approvals`" + ` listing. An id that is not waiting (it
expired, was spent, or never existed) is an error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runApprovalsApprove(cmd, host, args[0], yes) },
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "approve without asking, once you have read the call")

	return cmd
}

// runApprovalsApprove is `approvals approve id`: it fetches the queue,
// prints the call id names, asks (or takes --yes), and approves it. The
// queue is read and the call approved in two requests, so the question
// waits on no request's timeout. The approval carries the shown entry's
// created time, and the daemon refuses it (409) when the live entry id
// was created at another time, as after a restart, which empties the
// queue and its ids: the call shown is the call approved, or nothing is.
func runApprovalsApprove(cmd *cobra.Command, host Host, id string, yes bool) error {
	res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.ApprovalList, error) {
		return d.Approvals(ctx)
	})

	if err != nil {
		return err
	}

	i := slices.IndexFunc(res.Approvals, func(a admin.ApprovalRow) bool { return a.ID == id })

	if i < 0 {
		// The admin API's own 404, which approving the id would give.
		return fmt.Errorf("no approval %.64q is waiting: it expired, was spent, or never existed", id)
	}

	out := cmd.OutOrStdout()
	call := res.Approvals[i]

	if err := writeApprovalCall(out, call); err != nil {
		return err
	}

	// What was typed or pasted before the call was shown must not answer
	// for the owner: an agent can hand them "approve a3f2" and a "y" to
	// paste in one go.
	if tty, ok := terminal(cmd.InOrStdin()); ok && !yes {
		if err := discardTyped(int(tty.Fd())); err != nil {
			return fmt.Errorf("could not clear what was typed ahead, so nothing was asked or changed: pass --yes to approve it once you have reviewed it with `wayfindr approvals`: %w", err)
		}
	}

	// The question names the call again: 64 KiB of arguments can scroll the
	// call's first lines out of view.
	question := fmt.Sprintf("Approve %s for %s? [y/N] ", redact.Visible(call.Tool), redact.Visible(call.Client))
	ok, err := confirmChange(cmd.InOrStdin(), cmd.ErrOrStderr(), yes, question, false,
		"approve it once you have reviewed it with `wayfindr approvals`")

	if !ok {
		return err
	}

	created := call.Created
	_, err = tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (struct{}, error) {
		return struct{}{}, d.ApproveApproval(ctx, id, created)
	})

	if err != nil {
		return err
	}

	_, err = fmt.Fprintf(out, "Approved %s: the client's next call with the same tool and arguments runs, once.\n", redact.Printable(id))

	return err
}

// writeApprovalCall prints the call a approves, for the owner to read
// before they approve it: its client, tool, class, and state, then the
// size of its arguments and the arguments in full, on one line. Every value
// goes through redact.Visible, and the arguments through
// redact.VisibleArguments, as in writeApprovalsTable.
func writeApprovalCall(w io.Writer, a admin.ApprovalRow) error {
	_, err := fmt.Fprintf(w, "Client:  %s\nTool:    %s\nClass:   %s\nState:   %s\nArguments (%s): %s\n",
		redact.Visible(a.Client), redact.Visible(a.Tool), redact.Visible(a.Class), redact.Visible(string(a.State)),
		admin.FormatBytes(int64(len(a.Arguments))), redact.VisibleArguments(a.Arguments, ""))

	return err
}

// newApprovalsDenyCmd is `approvals deny <id>`. It asks nothing and shows
// nothing, so it names no created time: a denial lets nothing run, whatever
// call the id names.
func newApprovalsDenyCmd(host Host) *cobra.Command {
	short := "Deny a call: matching calls are refused until it expires"

	return &cobra.Command{
		Use:   "deny <id>",
		Short: short,
		Long: short + `.

The id is in the ` + "`wayfindr approvals`" + ` listing. An id that is not waiting (it
expired, was spent, or never existed) is an error.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (struct{}, error) {
				return struct{}{}, d.DenyApproval(ctx, args[0], time.Time{})
			})

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Denied %s: matching calls are refused until it expires.\n", redact.Printable(args[0]))

			return err
		},
	}
}

func runApprovalsList(cmd *cobra.Command, host Host) error {
	res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.ApprovalList, error) {
		return d.Approvals(ctx)
	})

	if err != nil {
		return err
	}

	if wantJSON(cmd) {
		return writeJSON(cmd.OutOrStdout(), res)
	}

	return writeApprovalsTable(cmd.OutOrStdout(), res, time.Now())
}

// writeApprovalsTable prints one row per approval, oldest first as the
// daemon lists them, each followed by a line of its id, the size of its
// arguments, and the arguments in full, or says there are none. AGE and
// EXPIRES are relative to now. Every cell and the arguments go through
// redact.Visible, as on the console's approvals page: a character that
// would act on the terminal or that the owner could not see, and a run of
// spaces that could push text out of view, are shown as escapes.
func writeApprovalsTable(w io.Writer, res admin.ApprovalList, now time.Time) error {
	var b bytes.Buffer

	if len(res.Approvals) == 0 {
		b.WriteString("No approvals are waiting.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"ID\tCLIENT\tTOOL\tCLASS\tSTATE\tAGE\tEXPIRES"}

		for _, a := range res.Approvals {
			rows = append(rows, strings.Join([]string{
				redact.Visible(a.ID), redact.Visible(a.Client), redact.Visible(a.Tool), redact.Visible(a.Class), redact.Visible(string(a.State)),
				approvalSpan(now.Sub(a.Created)), approvalSpan(a.Expires.Sub(now)),
			}, "\t"))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()

		// The arguments follow their row, under CLIENT, after the row's id:
		// arguments that wrap into what looks like a row of the table can't
		// also show the id of the row they belong to. They are added after
		// the table is laid out, so a long line can't widen a column. The
		// header is ASCII, so the offset of CLIENT in it is the column, in
		// characters, and tabwriter pads every id to less than that.
		lines := strings.SplitAfter(b.String(), "\n")
		column := strings.Index(lines[0], "CLIENT")
		b.Reset()

		for i, line := range lines {
			b.WriteString(line)

			if i > 0 && i <= len(res.Approvals) {
				// One line, in full: the owner approves what they read.
				a := res.Approvals[i-1]
				id := redact.Visible(a.ID)
				fmt.Fprintf(&b, "%s%s arguments (%s): %s\n", id, strings.Repeat(" ", max(column-utf8.RuneCountInString(id)-1, 1)),
					admin.FormatBytes(int64(len(a.Arguments))), redact.VisibleArguments(a.Arguments, ""))
			}
		}
	}

	_, err := w.Write(b.Bytes())

	return err
}

// approvalSpan is d as AGE and EXPIRES show it: "<1m" below a minute (and
// for a time that has passed), then whole minutes, then "1h5m" or "2h".
func approvalSpan(d time.Duration) string {
	m := int(d / time.Minute)

	switch {
	case m < 1:
		return "<1m"
	case m < 60:
		return fmt.Sprintf("%dm", m)
	case m%60 == 0:
		return fmt.Sprintf("%dh", m/60)
	default:
		return fmt.Sprintf("%dh%dm", m/60, m%60)
	}
}
