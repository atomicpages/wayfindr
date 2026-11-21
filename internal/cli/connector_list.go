package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/config"
)

// listedConnector is one row of `connector list`, and one element of its
// --json output.
type listedConnector struct {
	Name string `json:"name"`
	// Kind is "http" or "stdio".
	Kind string `json:"kind"`
	// Target is the URL, its query redacted, or the command line.
	Target string `json:"target"`
	// Auth is "oauth", "secret_header" when a header references a secret,
	// or "none".
	Auth string `json:"auth"`
}

// connectorList is the --json output of `connector list`.
type connectorList struct {
	Connectors []listedConnector `json:"connectors"`
}

func newConnectorListCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the connectors in the config file",
		Long: `List the connectors the config file defines, sorted by name: each one's kind
(http or stdio), its URL or command line, and how it authenticates (oauth, a
header that references a secret, or -). Only the file is read; the daemon is
not asked, so the list does not say whether a connector is connected. For
that, run "wayfindr status".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runConnectorList(cmd.OutOrStdout(), host, wantJSON(cmd))
		},
	}
	supportsJSON(cmd)

	return cmd
}

func runConnectorList(out io.Writer, host Host, asJSON bool) error {
	p, src, err := readConfigFile(host)

	if err != nil {
		return err
	}

	cfg, err := config.Parse(src, configOptions(host, p))

	if err != nil {
		// As unreadableListen words it, without asking a daemon: list reads
		// the file only.
		return fmt.Errorf("%w\n%s", err, "Fix the config file "+p.ConfigFile+".")
	}

	list := connectorList{Connectors: []listedConnector{}}

	for _, name := range sortedKeys(cfg.Connectors) {
		list.Connectors = append(list.Connectors, listConnector(name, cfg.Connectors[name]))
	}

	if asJSON {
		b, err := json.MarshalIndent(list, "", "  ")

		if err != nil {
			return err
		}

		_, err = out.Write(append(b, '\n'))

		return err
	}

	return writeConnectorList(out, list.Connectors)
}

// listConnector is the row of the connector c, called name.
func listConnector(name string, c config.Connector) listedConnector {
	l := listedConnector{
		Name:   name,
		Kind:   "http",
		Target: target(config.NewConnector{URL: c.URL, Transport: c.Transport, Command: c.Command, Args: c.Args}),
		Auth:   "none",
	}

	if c.IsStdio() {
		l.Kind = "stdio"
	}

	secretHeader := slices.ContainsFunc(sortedKeys(c.Headers), func(k string) bool {
		refs, err := c.Headers[k].Refs()

		return err == nil && slices.ContainsFunc(refs, func(r config.Ref) bool { return r.Kind == config.RefSecret })
	})

	switch {
	case c.UsesOAuth():
		l.Auth = "oauth"
	case secretHeader:
		l.Auth = "secret_header"
	}

	return l
}

// writeConnectorList prints one row per connector.
func writeConnectorList(w io.Writer, rows []listedConnector) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No connectors are configured.")

		return err
	}

	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	lines := []string{"NAME\tKIND\tTARGET\tAUTH"}

	for _, r := range rows {
		auth := r.Auth

		switch r.Auth {
		case "secret_header":
			auth = "header (secret)"
		case "none":
			auth = "-"
		}

		lines = append(lines, r.Name+"\t"+r.Kind+"\t"+r.Target+"\t"+auth)
	}

	// A tabwriter over a bytes.Buffer cannot fail.
	_, _ = io.WriteString(tw, strings.Join(lines, "\n")+"\n")
	_ = tw.Flush()
	_, err := w.Write(b.Bytes())

	return err
}
