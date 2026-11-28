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

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// toolsTimeout bounds `connector tools`: one tools/list to the upstream.
const toolsTimeout = 30 * time.Second

func newConnectorToolsCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools <name> [tool]",
		Short: "List a connector's tools, or describe one tool's arguments",
		Long: `Ask the running daemon for every tool the connector's upstream serves now,
with the tool's class and whether the connector's tools list exposes it to
agents, and how many arguments it requires. Use it to write the tools list of
[connectors.<name>], or to pick a tool to try with ` + "`wayfindr call`" + `.

The class comes from the upstream's annotations: read (readOnlyHint), write
(destructiveHint false), or destructive (everything else, including tools
with no annotations). Annotations are hints from the upstream, not
guarantees, so name the writes you allow rather than trusting @write.

With a tool, it describes that tool's arguments instead: each one's name,
type, whether the schema requires it, and its description, wrapped to the
terminal (one line per argument when piped). ` + "`wayfindr call <connector.tool> --scaffold`" + `
prints a JSON object of the required ones to start from.`,
		Example: `  wayfindr connector tools linear
  wayfindr connector tools linear save_issue`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), toolsTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			list, err := d.Tools(ctx, args[0])

			if err != nil {
				return d.explain(err)
			}

			if len(args) == 2 {
				return writeToolArgs(cmd, list, args[1])
			}

			if wantJSON(cmd) {
				b, err := json.MarshalIndent(list, "", "  ")

				if err != nil {
					return err
				}

				_, err = cmd.OutOrStdout().Write(append(b, '\n'))

				return err
			}

			return writeToolsTable(cmd.OutOrStdout(), list)
		},
	}
	supportsJSON(cmd)

	return cmd
}

// writeToolsTable prints one row per tool, then how many are exposed. Tool
// names come from the upstream, so they are made printable first: a name
// could carry an escape sequence or a bidi override.
func writeToolsTable(w io.Writer, list admin.ToolList) error {
	var b bytes.Buffer

	if len(list.Tools) == 0 {
		fmt.Fprintf(&b, "%s serves no tools.\n", list.Connector)
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"TOOL\tCLASS\tEXPOSED\tREQUIRED ARGS"}
		exposed := 0

		for _, t := range list.Tools {
			mark := "no"

			if t.Exposed {
				mark = "yes"
				exposed++
			}

			rows = append(rows, redact.Printable(t.Name)+"\t"+t.Class+"\t"+mark+"\t"+strconv.Itoa(t.RequiredArgs))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
		fmt.Fprintf(&b, "\n%d of %d tools exposed.\n", exposed, len(list.Tools))
	}

	_, err := w.Write(b.Bytes())

	return err
}

// writeToolArgs prints the arguments of list's tool called name: a line
// naming it and its class, a note when its schema requires nothing (a tool
// that both creates and updates often says what a create needs only in its
// descriptions), and argTable, wrapped to stdout's width on a terminal.
// With --json it prints the tool's entry, definition included.
func writeToolArgs(cmd *cobra.Command, list admin.ToolList, name string) error {
	i := slices.IndexFunc(list.Tools, func(t admin.ToolEntry) bool { return t.Name == name })

	if i < 0 {
		return fmt.Errorf("%s serves no tool called %s: list them with `wayfindr connector tools %s`",
			list.Connector, redact.Printable(name), list.Connector)
	}

	t := list.Tools[i]
	out := cmd.OutOrStdout()

	if wantJSON(cmd) {
		return writeJSON(out, t)
	}

	fields, err := inputFields(t.Definition)

	if err != nil {
		return err
	}

	var b strings.Builder

	fmt.Fprintf(&b, "%s.%s (%s", list.Connector, redact.Printable(t.Name), t.Class)

	if !t.Exposed {
		b.WriteString(", hidden by the tools list")
	}

	b.WriteString(")\n")

	switch {
	case len(fields) == 0:
		b.WriteString("It takes no arguments.\n")
	default:
		if !slices.ContainsFunc(fields, func(f admin.SchemaField) bool { return f.Required }) {
			b.WriteString("No argument is required by the schema; its descriptions may still say what a call needs.\n")
		}

		b.WriteString("\n" + argTable(fields, termWidth(out)))
	}

	_, err = io.WriteString(out, b.String())

	return err
}

// termWidth is w's width in columns when it is a terminal, else 0.
func termWidth(w io.Writer) int {
	r, _ := w.(io.Reader) // an *os.File is both

	f, ok := terminal(r)

	if !ok {
		return 0
	}

	width, _, err := term.GetSize(int(f.Fd()))

	if err != nil {
		return 0
	}

	return width
}
