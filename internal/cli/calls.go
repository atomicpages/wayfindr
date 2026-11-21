package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/store"
)

// callsTimeout bounds the calls request; the daemon answers from the store.
const callsTimeout = 10 * time.Second

func newCallsCmd(host Host) *cobra.Command {
	var (
		connectors []string
		since      = lookback(24 * time.Hour)
		errorsOnly bool
		limit      int
	)
	cmd := &cobra.Command{
		Use:   "calls",
		Short: "Show the tools agents called through Wayfindr",
		Long: `Ask the running daemon for its call log: one row per call_tool call, newest
first, with when it started, the client, the client token it authenticated
with, the tool, how long the upstream took, and how it ended. A call that
did not end ok is followed by the error text the agent got back. Arguments
and results are never stored.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if since < 0 {
				return fmt.Errorf("--since must not be negative")
			}

			if limit < 1 || limit > admin.MaxCallsLimit {
				return fmt.Errorf("--limit must be from 1 to %d", admin.MaxCallsLimit)
			}

			q := admin.CallsQuery{Connectors: connectors, ErrorsOnly: errorsOnly, Limit: limit}

			if since > 0 {
				q.Since = time.Now().Add(-time.Duration(since))
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), callsTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			res, err := d.Calls(ctx, q)

			if err != nil {
				return d.explain(err)
			}

			if wantJSON(cmd) {
				return writeCallsJSON(cmd.OutOrStdout(), res)
			}

			return writeCallsTable(cmd.OutOrStdout(), res)
		},
	}
	cmd.Flags().StringArrayVar(&connectors, "connector", nil, "only calls to this connector (repeatable)")
	cmd.Flags().Var(&since, "since", "how far back to look, such as 90m, 36h, 7d, or 1d12h; 0 for the whole log")
	cmd.Flags().BoolVar(&errorsOnly, "errors", false, "only calls whose result is not ok")
	cmd.Flags().IntVar(&limit, "limit", admin.DefaultCallsLimit, "at most this many calls, newest first")
	supportsJSON(cmd)

	return cmd
}

// day is what d stands for in --since: 24 hours, not a calendar day.
const day = 24 * time.Hour

// lookback is the --since flag: a Go duration, which may lead with a whole
// number of days, as in 7d or 1d12h.
type lookback time.Duration

func (l *lookback) Set(s string) error {
	d, err := parseLookback(s)

	if err != nil {
		return err
	}

	*l = lookback(d)

	return nil
}

func (l *lookback) String() string { return time.Duration(*l).String() }

func (l *lookback) Type() string { return "duration" }

// errLookback is a --since value that is not a duration.
var errLookback = errors.New("want a duration such as 90m, 36h, 7d, or 1d12h")

// parseLookback reads a Go duration (time.ParseDuration), optionally led by
// a sign and a whole number of days: "7d", "1d12h", "-1d". The days come
// first, so "12h1d" is refused, as is a sign after them.
func parseLookback(s string) (time.Duration, error) {
	rest, neg := strings.CutPrefix(s, "-")

	if !neg {
		rest = strings.TrimPrefix(rest, "+")
	}

	var days int64
	numDays, after, hasDays := strings.Cut(rest, "d")

	if hasDays {
		n, err := strconv.ParseInt(numDays, 10, 64)

		// ParseInt takes a sign; days are digits only.
		if err != nil || n < 0 || numDays[0] < '0' || numDays[0] > '9' || n > int64(math.MaxInt64/day) {
			return 0, errLookback
		}

		days, rest = n, after
	}

	var d time.Duration

	if rest != "" || !hasDays {
		var err error

		if rest == "" || rest[0] == '+' || rest[0] == '-' {
			return 0, errLookback
		}

		if d, err = time.ParseDuration(rest); err != nil {
			return 0, errLookback
		}
	}

	total := time.Duration(days) * day

	if d > math.MaxInt64-total {
		return 0, errLookback
	}

	total += d

	if neg {
		total = -total
	}

	return total, nil
}

func writeCallsJSON(w io.Writer, res admin.Calls) error {
	b, err := json.MarshalIndent(res, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}

// writeCallsTable prints one row per call, newest first, and says so when
// there are none or the call log is off. Times are UTC.
func writeCallsTable(w io.Writer, res admin.Calls) error {
	var b bytes.Buffer

	if len(res.Calls) == 0 {
		b.WriteString("No calls match.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"TIME\tCLIENT\tTOKEN\tTOOL\tDURATION\tRESULT"}

		for _, c := range res.Calls {
			rows = append(rows, strings.Join([]string{
				formatTime(c.StartedAt), orDash(c.Client), orDash(c.TokenName), callTool(c), callDuration(c.DurationMS), callResult(c),
			}, "\t"))

			// Under the result, after an empty cell per column so the
			// columns stay aligned: the last cell sets no width. The daemon
			// stores the text printable and on one line.
			if c.Error != "" {
				rows = append(rows, "\t\t\t\t\t"+c.Error)
			}
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	if res.Retention == "0s" {
		b.WriteString("\nThe call log is off: [calls] retention is 0s, so no calls are recorded.\n")
	}

	_, err := w.Write(b.Bytes())

	return err
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}

	return s
}

// callTool is the qualified name as the agent wrote it.
func callTool(c admin.Call) string {
	if c.Connector == "" {
		return orDash(c.Tool)
	}

	return c.Connector + "." + c.Tool
}

// callDuration renders milliseconds below a second, and tenths of a
// second from there.
func callDuration(ms *int64) string {
	switch {
	case ms == nil:
		return "-"
	case *ms < 1000:
		return fmt.Sprintf("%dms", *ms)
	default:
		return fmt.Sprintf("%.1fs", float64(*ms)/1000)
	}
}

// callResult is the result, followed for a call that is not ok by its
// Case, or its Kind when it has none.
func callResult(c admin.Call) string {
	why := c.Case

	if why == "" {
		why = c.Kind
	}

	if c.Result == store.CallOK || why == "" {
		return string(c.Result)
	}

	return string(c.Result) + ": " + why
}
