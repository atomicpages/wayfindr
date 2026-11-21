package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

func newScopeCmd(host Host) *cobra.Command {
	var profileName string

	cmd := &cobra.Command{
		Use:   "scope [connector]",
		Short: "Show the project scope that applies here and what it exposes",
		Long: `Find the project scope that ` + "`wayfindr stdio`" + ` started in this directory would
send: the nearest .wayfindr.toml here or in a parent, below your home
directory. Check that it is valid, and show which configured connectors it
lets agents use.

With a connector, list its tools as ` + "`wayfindr connector tools`" + ` does, where a tool
is exposed only if both the config's tools list and the scope's expose it.

A scope only narrows: it lists the connectors a project may use, and tools
lists that apply on top of the config's.

With --profile, also apply the config's profile of that name, as the running
daemon has it ([profiles.<name>], ADR 0011): a connector or tool must pass the
config, the profile, and the scope file, if any.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := host.workDir()

			if err != nil {
				return fmt.Errorf("working directory: %w", err)
			}

			// Without a home directory the walk goes to the root, as the
			// shim's does.
			var home string

			if host.Env.HomeDir != nil {
				home, _ = host.Env.HomeDir()
			}

			scope, file, err := config.LoadScope(dir, home)

			if err != nil {
				return err
			}

			w := cmd.OutOrStdout()
			asJSON := wantJSON(cmd)

			if scope == nil && profileName == "" {
				if asJSON {
					return writeJSON(w, emptyScopeReport(dir, args))
				}

				_, err := fmt.Fprintf(w, "No project scope applies in %s: agents get every connector and tool the config exposes.\n", dir)

				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), toolsTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			v := scopeView{file: file, n: config.Narrowing{File: scope}}
			// The status is read once: for the profile, as the running
			// daemon applies it, and for the configured connectors.
			var status *admin.Status

			if profileName != "" {
				s, err := d.Status(ctx)

				if err != nil {
					return d.explain(err)
				}

				p, ok := s.Profiles[profileName]

				if !ok {
					return unknownProfile(profileName, s.Profiles)
				}

				v.n.ProfileName, v.n.Profile, status = profileName, &p, &s
			}

			if len(args) == 1 {
				list, err := d.Tools(ctx, args[0])

				if err != nil {
					return d.explain(err)
				}

				for i, t := range list.Tools {
					class := config.ToolClass(t.Class)
					list.Tools[i].Exposed = t.Exposed && v.n.Hides(list.Connector, t.Name, class) == config.SourceNone
				}

				if asJSON {
					return writeJSON(w, scopeToolsReport{
						Dir: dir, Profile: profileName, ScopeFile: file,
						Connector: list.Connector, Tools: list.Tools,
					})
				}

				var b bytes.Buffer

				v.writeHeader(&b)

				if _, err := w.Write(b.Bytes()); err != nil {
					return err
				}

				return writeToolsTable(w, list)
			}

			if status == nil {
				s, err := d.Status(ctx)

				if err != nil {
					return d.explain(err)
				}

				status = &s
			}

			if asJSON {
				return writeJSON(w, newScopeReport(dir, v, status.Connectors))
			}

			return writeScope(w, v, status.Connectors)
		},
	}
	cmd.Flags().StringVar(&profileName, "profile", "", "also apply the config's profile `name` ([profiles.<name>])")
	supportsJSON(cmd)

	return cmd
}

// unknownProfile is the error for a --profile the running config doesn't
// define. It names the ones it does, so a typo is easy to spot.
func unknownProfile(name string, profiles map[string]config.Scope) error {
	if len(profiles) == 0 {
		return fmt.Errorf("profile %q: the running config defines no profiles; add one as [profiles.<name>]", name)
	}

	return fmt.Errorf("profile %q: the running config doesn't define it; it defines %s", name, strings.Join(slices.Sorted(maps.Keys(profiles)), ", "))
}

// scopeView is what narrows the agents a client starts here, n, and the
// path of the scope file that is part of it.
type scopeView struct {
	n config.Narrowing
	// file is the scope file's path; "" for none.
	file string
}

// writeHeader names the profile and the scope file, then a blank line.
func (v scopeView) writeHeader(b *bytes.Buffer) {
	if v.n.ProfileName != "" {
		fmt.Fprintf(b, "Profile: %s\n", v.n.ProfileName)
	}

	if v.file != "" {
		fmt.Fprintf(b, "Project scope: %s\n", v.file)
	}

	b.WriteString("\n")
}

// scopeTools is the tools column of s for connector: "-" when s leaves the
// connector out, its tools list there, or "as configured" when it has none.
// A scope file may come from a cloned repository, so its entries are made
// printable as upstream names are; a profile's are too, for one rule.
func scopeTools(s *config.Scope, connector string) string {
	if !s.Allows(connector) {
		return "-"
	}

	if s != nil {
		if p, ok := s.Tools[connector]; ok {
			return redact.Printable(strings.Join(p, ", "))
		}
	}

	return "as configured"
}

// writeScope prints the profile and the scope file, one row per configured
// connector with whether both let agents use it and each one's tools list
// there, then a warning for each connector the scope file names that is not
// configured. A column is left out for the one v lacks.
func writeScope(w io.Writer, v scopeView, connectors []admin.Connector) error {
	var b bytes.Buffer

	v.writeHeader(&b)

	if len(connectors) == 0 {
		b.WriteString("No connectors are configured.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		header := "CONNECTOR\tIN SCOPE"

		if v.n.ProfileName != "" {
			header += "\tPROFILE TOOLS"
		}

		if v.file != "" {
			header += "\tSCOPE TOOLS"
		}

		rows := []string{header}

		for _, c := range connectors {
			row := c.Name + "\tno"

			if v.n.Allows(c.Name) {
				row = c.Name + "\tyes"
			}

			// Each column is what its own scope says, so a "no" shows which
			// one leaves the connector out.
			if v.n.ProfileName != "" {
				row += "\t" + scopeTools(v.n.Profile, c.Name)
			}

			if v.file != "" {
				row += "\t" + scopeTools(v.n.File, c.Name)
			}

			rows = append(rows, row)
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	configured := func(name string) bool {
		return slices.ContainsFunc(connectors, func(c admin.Connector) bool { return c.Name == name })
	}

	// A profile's connectors are checked when the config loads; only the
	// scope file's can name one this user lacks.
	var named []string

	if v.n.File != nil {
		named = v.n.File.Connectors
	}

	for _, name := range named {
		if !configured(name) {
			fmt.Fprintf(&b, "\nWarning: the scope names %s, which is not configured; it has no effect.\n", name)
		}
	}

	_, err := w.Write(b.Bytes())

	return err
}
