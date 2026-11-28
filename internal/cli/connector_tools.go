package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// toolsTimeout bounds `connector tools`: one tools/list to the upstream.
const toolsTimeout = 30 * time.Second

func newConnectorToolsCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tools <name>",
		Short: "List a connector's tools and which ones its tools list exposes",
		Long: `Ask the running daemon for every tool the connector's upstream serves now,
with the tool's class and whether the connector's tools list exposes it to
agents, and how many arguments it requires. Use it to write the tools list of
[connectors.<name>], or to pick a tool to try with ` + "`wayfindr call`" + `.

The class comes from the upstream's annotations: read (readOnlyHint), write
(destructiveHint false), or destructive (everything else, including tools
with no annotations). Annotations are hints from the upstream, not
guarantees, so name the writes you allow rather than trusting @write.`,
		Args: cobra.ExactArgs(1),
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
