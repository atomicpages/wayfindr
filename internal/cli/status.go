package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/upstream"
)

// statusTimeout bounds the status request; the daemon answers from memory
// and the store.
const statusTimeout = 10 * time.Second

// exitUnhealthy is the exit code of `status --check` when something needs
// attention; 1 stays the code for a status that could not be read.
const exitUnhealthy = 2

func newStatusCmd(host Host) *cobra.Command {
	var check bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show each connector's state, tools, and last sync",
		Long: `Ask the running daemon for each connector's state and what to do about it,
its tool count and last sync, credentials left behind by removed connectors,
and the error of a config the daemon could not load.

With --check the output is the same, and the exit code says whether
anything needs attention: 2 when a connector is not ready, a connector has a
warning (notes don't count), or the daemon reports a config error; 0
otherwise. 1 still means the status could not be read.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), statusTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			s, err := d.Status(ctx)

			if err != nil {
				return d.explain(err)
			}

			write := writeStatusTable

			if wantJSON(cmd) {
				write = writeStatusJSON
			}

			if err := write(cmd.OutOrStdout(), s); err != nil {
				return err
			}

			// Beside the warnings, but not in the --json body, which is the
			// daemon's status as it sent it.
			if !wantJSON(cmd) {
				if err := writeApprovalsWaiting(cmd.OutOrStdout(), waitingApprovals(ctx, d)); err != nil {
					return err
				}
			}

			if check && needsAttention(s) {
				return exitWith(cmd, exitUnhealthy)
			}

			return nil
		},
	}
	cmd.Flags().BoolVar(&check, "check", false, "exit 2 when a connector is not ready or has a warning, or the config has an error")
	supportsJSON(cmd)

	return cmd
}

// needsAttention reports what `status --check` exits 2 on: a connector that
// is not ready, a warning that is not a note, or a config error. Orphaned
// credentials and notes don't count: nothing is broken yet.
func needsAttention(s admin.Status) bool {
	if s.ConfigError != "" {
		return true
	}

	return slices.ContainsFunc(s.Connectors, func(c admin.Connector) bool {
		return c.State != upstream.StateReady ||
			slices.ContainsFunc(c.Warnings, func(w admin.Warning) bool { return !w.Note })
	})
}

// waitingApprovals is how many approvals wait for the owner's answer: the
// queue's pending entries. A granted entry waits for the agent, and a denied
// one for nothing. It is 0 when the queue can't be read: an answer to
// `status` is not worth failing for a count beside it, and a daemon that
// can't list approvals has none to show. The error is logged at debug.
func waitingApprovals(ctx context.Context, d *adminDaemon) int {
	list, err := d.Approvals(ctx)

	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).Msg("count waiting approvals")

		return 0
	}

	n := 0

	for _, a := range list.Approvals {
		if a.State == admin.ApprovalPending {
			n++
		}
	}

	return n
}

// writeApprovalsWaiting prints "1 approval waiting", and where to see it,
// when n is above 0, and nothing otherwise.
func writeApprovalsWaiting(w io.Writer, n int) error {
	if n == 0 {
		return nil
	}

	noun, pronoun := "approvals", "them"

	if n == 1 {
		noun, pronoun = "approval", "it"
	}

	_, err := fmt.Fprintf(w, "\n%d %s waiting. Review %s with `wayfindr approvals`.\n", n, noun, pronoun)

	return err
}

func writeStatusJSON(w io.Writer, s admin.Status) error {
	b, err := json.MarshalIndent(s, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}

// writeStatusTable prints one row per connector, then what to do about each
// connector that needs attention, the warnings and notes, the orphaned
// credentials, and the config error. Times are UTC.
func writeStatusTable(w io.Writer, s admin.Status) error {
	var b bytes.Buffer

	if len(s.Connectors) == 0 {
		b.WriteString("No connectors are configured.\n")
	} else {
		writeConnectorTable(&b, s.Connectors)
	}

	for _, c := range s.Connectors {
		writeConnectorDetail(&b, c)
	}

	writeWarnings(&b, s.Connectors, "Warnings:", false)
	writeWarnings(&b, s.Connectors, "Notes:", true)

	if len(s.OrphanedCredentials) > 0 {
		fmt.Fprintf(&b, "\nOrphaned credentials (connectors no longer in the config): %s\n", strings.Join(s.OrphanedCredentials, ", "))
		b.WriteString("  Remove each with `wayfindr auth logout <connector>`.\n")
	}

	if s.ConfigError != "" {
		b.WriteString("\nConfig error: the daemon keeps running its last good config.\n")
		fmt.Fprintf(&b, "  %s\n", s.ConfigError)
	}

	_, err := w.Write(b.Bytes())

	return err
}

// toolCount is the TOOLS cell: the catalog entries, and how many the
// connector's tools list hid, if any, as in "38 (4 hidden)".
func toolCount(c admin.Connector) string {
	if c.HiddenTools == 0 {
		return strconv.Itoa(c.Tools)
	}

	return fmt.Sprintf("%d (%d hidden)", c.Tools, c.HiddenTools)
}

func writeConnectorTable(b *bytes.Buffer, cs []admin.Connector) {
	// The expiry column appears once some connector has a token expiry.
	expiry := slices.ContainsFunc(cs, func(c admin.Connector) bool { return c.TokenExpiresAt != nil })
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	header := "CONNECTOR\tSTATE\tTOOLS\tLAST SYNC"

	if expiry {
		header += "\tTOKEN EXPIRES"
	}

	rows := []string{header}

	for _, c := range cs {
		row := fmt.Sprintf("%s\t%s\t%s\t%s", c.Name, c.State, toolCount(c), lastSync(c.LastSync))

		if expiry {
			exp := "-"

			if c.TokenExpiresAt != nil {
				exp = formatTime(*c.TokenExpiresAt)
			}

			row += "\t" + exp
		}

		rows = append(rows, row)
	}

	// A tabwriter over a bytes.Buffer cannot fail.
	_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
	_ = tw.Flush()
}

// writeConnectorDetail explains a connector that is neither starting nor
// ready, or whose last sync failed.
func writeConnectorDetail(b *bytes.Buffer, c admin.Connector) {
	failed := c.LastSync != nil && c.LastSync.Error != ""
	settled := c.State == upstream.StateReady || c.State == upstream.StateStarting

	if settled && !failed {
		return
	}

	b.WriteString("\n")
	renderConnector(b, c)

	if failed {
		fmt.Fprintf(b, "  Last sync failed: %s\n", c.LastSync.Error)
	}
}

// writeWarnings lists, under heading, the connectors' warnings that are
// notes, or those that are not. It writes nothing when there are none.
func writeWarnings(b *bytes.Buffer, cs []admin.Connector, heading string, notes bool) {
	started := false

	for _, c := range cs {
		for _, w := range c.Warnings {
			if w.Note != notes {
				continue
			}

			if !started {
				fmt.Fprintf(b, "\n%s\n", heading)
				started = true
			}

			renderAdvice(b, "  ", c.Name, w.Advice)
		}
	}
}

// renderConnector writes one connector as a user reads it: its advice's
// summary, then the action and the link that fix it, indented; or that it
// is ready. Every command that reports a connector's state (status, secret
// set, auth, connector add) prints it through here.
func renderConnector(b *bytes.Buffer, c admin.Connector) {
	a := c.Advice

	if a == nil {
		// The daemon advises on every state but ready.
		fmt.Fprintf(b, "%s is %s.\n", c.Name, c.State)

		return
	}

	renderAdvice(b, "", c.Name, *a)
}

// renderAdvice writes a, about connector, at indent: the summary, then the
// action and the link, indented one step further.
func renderAdvice(b *bytes.Buffer, indent, connector string, a admin.Advice) {
	fmt.Fprintf(b, "%s%s: %s\n", indent, connector, a.Summary)

	for _, l := range []string{a.Action, a.Link} {
		if l != "" {
			fmt.Fprintf(b, "%s  %s\n", indent, l)
		}
	}
}

// renderSettled writes a connector that has just connected: ready with its
// tool count, or what renderConnector says.
func renderSettled(b *bytes.Buffer, c admin.Connector) {
	if c.State == upstream.StateReady {
		fmt.Fprintf(b, "%s is ready (%d tools).\n", c.Name, c.Tools)

		return
	}

	renderConnector(b, c)
}

func lastSync(s *admin.Sync) string {
	switch {
	case s == nil:
		return "never"
	case s.Error != "":
		return formatTime(s.At) + " (failed)"
	default:
		return formatTime(s.At)
	}
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }
