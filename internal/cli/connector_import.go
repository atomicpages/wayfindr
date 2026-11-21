package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/clientconfig"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
	"github.com/atomicpages/wayfindr/internal/redact"
)

// importFlags are the flags of `connector import`.
type importFlags struct {
	dryRun   bool
	yes      bool
	file     string
	plainEnv []string
	noOAuth  bool
	noWait   bool
	// includeProject reads the MCP files in the current directory too.
	includeProject bool
}

func newConnectorImportCmd(host Host) *cobra.Command {
	var f importFlags
	names := make([]string, 0, len(clientconfig.Clients()))

	for _, c := range clientconfig.Clients() {
		names = append(names, string(c))
	}

	cmd := &cobra.Command{
		Use:   "import <client>",
		Short: "Add the MCP servers a client is set up with as connectors",
		Long: `Read the MCP servers of a client (` + strings.Join(names, ", ") + `)
from its config files and add them as connectors, in one edit of the config
file. The client's files are only read, never changed.

Literal values in env and headers are stored as secrets, as are ${VAR} values
this shell sets; the config file refers to them as ${secret:…}. --plain-env
keeps a variable as plain text. A ${VAR} this shell doesn't set, and every
${VAR} in args or a url, becomes ${env:VAR}, which the daemon's environment
must set. Entries that are turned off, that point at Wayfindr itself, whose
name is taken, or that set something a connector can't express are skipped.

The files in the current directory (.cursor/mcp.json, .vscode/mcp.json,
.mcp.json, .codex/config.toml) come with the project, so whoever wrote it
chose their servers: they are read only with --include-project, and only for
a project you trust.

--dry-run shows what would be added, asks no server anything, and changes
nothing. Otherwise the command asks once on a terminal (or takes --yes); only
then does it ask each HTTP server whether it uses OAuth, store the secrets,
write the connectors, and report each one as connector add does.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := clientconfig.ParseClient(args[0])

			if err != nil {
				return err
			}

			return runConnectorImport(cmd, host, c, f)
		},
	}

	fl := cmd.Flags()
	fl.BoolVar(&f.dryRun, "dry-run", false, "show what would be added; change nothing")
	fl.BoolVarP(&f.yes, "yes", "y", false, "don't ask before adding")
	fl.StringVar(&f.file, "file", "", "read this one `file`, in the client's format, instead of the client's own files")
	fl.StringArrayVar(&f.plainEnv, "plain-env", nil, "an environment variable `K` to keep as plain text instead of a secret (repeatable)")
	fl.BoolVar(&f.noOAuth, "no-oauth", false, "don't ask HTTP servers whether they use OAuth")
	fl.BoolVar(&f.noWait, "no-wait", false, "don't wait for the connectors; without secrets to store, don't start the daemon either")
	fl.BoolVar(&f.includeProject, "include-project", false, "also read the MCP files in the current directory: only for a project you trust")

	return cmd
}

func runConnectorImport(cmd *cobra.Command, host Host, client clientconfig.Client, f importFlags) error {
	ctx := cmd.Context()
	in, out, errOut := cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	p, src, err := readConfigFile(host)

	if err != nil {
		return err
	}

	opts := configOptions(host, p)
	cfg, err := config.Parse(src, opts)

	if err != nil {
		return editError(&config.Error{Kind: config.KindUnsafe, Case: config.CaseUnparsable, Err: err})
	}

	dirs, err := clientDirs(host, f.file != "")

	if err != nil {
		return err
	}

	dirs.Project = f.includeProject

	found, err := readClient(client, dirs, f.file)

	if err != nil {
		return err
	}

	lookupEnv := host.LookupEnv

	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	plan := clientconfig.Map(found.Servers, clientconfig.Options{
		Existing: cfg.Connectors, Listen: cfg.Server.Listen, LookupEnv: lookupEnv, PlainEnv: f.plainEnv,
		Secrets: cfg.SecretRefs(),
	})

	next, err := appendAll(src, &plan, opts)

	if err != nil {
		return editError(err)
	}

	var b bytes.Buffer
	renderImportPlan(&b, dirs.Home, found, plan, !f.noOAuth)

	if _, err := out.Write(b.Bytes()); err != nil {
		return err
	}

	n := len(plan.Connectors)

	switch {
	case n == 0:
		_, err := fmt.Fprintln(out, "Nothing to import.")

		return err
	case f.dryRun:
		_, err := fmt.Fprintf(out, "Run again without --dry-run to add %s.\n", countConnectors(n))

		return err
	}

	if ok, err := confirmChange(in, out, f.yes, fmt.Sprintf("Add %s? [Y/n] ", countConnectors(n)), true, "add them"); !ok {
		return err
	}

	// Only now is any server asked anything: the plan came from files
	// someone else may have written, so a dry run, or a plan turned down,
	// sends no request (wayfindr-awvh). A connector found to use OAuth is
	// written with its auth table, so the edit is made again.
	if !f.noOAuth {
		set, err := askOAuth(ctx, out, &plan)

		if err != nil {
			return err
		}

		if set {
			skipped := len(plan.Skips)

			if next, err = appendAll(src, &plan, opts); err != nil {
				return editError(err)
			}

			// A connector its auth table made invalid is no longer added:
			// say so, since the confirm counted it.
			for _, sk := range plan.Skips[skipped:] {
				if _, err := fmt.Fprintf(out, "Skipped %s: %s\n", redact.Printable(sk.From.Name), redact.Printable(sk.Reason)); err != nil {
					return err
				}
			}

			if n = len(plan.Connectors); n == 0 {
				_, err := fmt.Fprintln(out, "Nothing to import.")

				return err
			}
		}
	}

	secrets, err := importSecrets(in, errOut, plan, found.Inputs)

	if err != nil {
		return err
	}

	var d *adminDaemon
	var prevConfigErr string

	if len(secrets) > 0 || !f.noWait {
		if d, prevConfigErr, err = startDaemon(ctx, host, p); err != nil {
			return err
		}
	}

	var stored []string
	keptNote := func(err error) error {
		if len(stored) == 0 {
			return err
		}

		return fmt.Errorf("%w (the secrets %s were stored and are kept; the config file is unchanged)", err, strings.Join(stored, ", "))
	}

	for _, s := range secrets {
		if err := putSecret(ctx, d, s); err != nil {
			return keptNote(err)
		}

		stored = append(stored, s.name)

		if _, err := fmt.Fprintf(out, "Stored secret %s.\n", s.name); err != nil {
			return err
		}
	}

	names := make([]string, 0, n)

	for _, pc := range plan.Connectors {
		names = append(names, pc.Connector.Name)
	}

	if err := newfile.Replace(p.ConfigFile, src, next, p.ConfigFile); err != nil {
		return keptNote(editError(err))
	}

	if _, err := fmt.Fprintf(out, "Added %s: %s.\n", countConnectors(n), strings.Join(names, ", ")); err != nil {
		return err
	}

	if f.noWait {
		if err := writeTakesEffect(ctx, host, out, p); err != nil {
			return err
		}
	} else {
		for _, name := range names {
			if err := reportConnector(ctx, cmd, host, d, name, reportOpts{prevConfigErr: prevConfigErr}); err != nil {
				return err
			}
		}
	}

	return writeRemoveHints(out, client, plan)
}

// askOAuth asks each planned HTTP server that has no auth table and no
// headers whether it uses OAuth, as connector add does, and gives the ones
// that do auth = { type = "oauth" }. It reports whether it gave any.
func askOAuth(ctx context.Context, out io.Writer, plan *clientconfig.Plan) (bool, error) {
	set := false

	for i := range plan.Connectors {
		c := &plan.Connectors[i].Connector

		if !mayUseOAuth(*c) {
			continue
		}

		fix := fmt.Sprintf("If it needs OAuth, add auth = { type = \"oauth\" } to [connectors.%s] after the import.", c.Name)
		auth, err := detectOAuth(ctx, out, *c, oauthAdvice{unchecked: fix, unclear: fix})

		if err != nil {
			return set, err
		}

		c.Auth = auth
		set = set || auth != nil
	}

	return set, nil
}

// mayUseOAuth reports whether import asks c's server whether it uses OAuth:
// an HTTP server with no auth table and no headers of its own.
func mayUseOAuth(c config.NewConnector) bool {
	return c.URL != "" && c.Auth == nil && len(c.Headers) == 0
}

// clientDirs are where the client's files are looked for: the home
// directory and the working directory (Host.Getwd). With --file the home
// directory is not needed, so failing to find it is not an error.
func clientDirs(host Host, haveFile bool) (clientconfig.Dirs, error) {
	cwd, err := host.workDir()

	if err != nil {
		return clientconfig.Dirs{}, fmt.Errorf("find the current directory: %w", err)
	}

	home, err := host.Env.HomeDir()

	if err != nil && !haveFile {
		return clientconfig.Dirs{}, fmt.Errorf("find the home directory: %w", err)
	}

	return clientconfig.Dirs{Home: home, Cwd: cwd, GOOS: runtime.GOOS, Getenv: host.Env.Getenv}, nil
}

func readClient(c clientconfig.Client, d clientconfig.Dirs, file string) (clientconfig.Found, error) {
	if file != "" {
		return clientconfig.ReadFile(c, file, d)
	}

	found, err := clientconfig.Read(c, d)

	if err == nil && len(found.Files) == 0 && len(found.SkippedProject) == 0 {
		err = fmt.Errorf("found no %s config here or in your home directory; --file reads one file in its format", c)
	}

	return found, err
}

// appendAll adds every planned connector to src, one AppendConnector each,
// so the result is one edit that still adds nothing but them. A connector
// the config would refuse on its own is moved to the skips.
func appendAll(src []byte, plan *clientconfig.Plan, opts config.Options) ([]byte, error) {
	next := src
	var kept []clientconfig.Planned

	for _, pc := range plan.Connectors {
		n, err := config.AppendConnector(next, pc.Connector, opts)
		var invalid *config.ValidationError

		if errors.As(err, &invalid) {
			plan.Skips = append(plan.Skips, clientconfig.Skip{From: pc.From, Reason: invalid.Error()})

			continue
		}

		if err != nil {
			return nil, err
		}

		next = n
		kept = append(kept, pc)
	}

	plan.Connectors = kept

	return next, nil
}

// importSecrets prompts for the VS Code inputs the planned secrets use,
// each once, and returns every secret with its value.
func importSecrets(in io.Reader, prompt io.Writer, plan clientconfig.Plan, inputs map[string]clientconfig.Input) ([]pendingSecret, error) {
	ids := plan.Inputs()

	if len(ids) > 1 {
		if _, ok := terminal(in); !ok {
			return nil, errors.New("more than one input can't be piped: run this on a terminal")
		}
	}

	values := map[string]string{}

	for _, id := range ids {
		// The id and description come from the client's file, like the plan
		// (wayfindr-awvh): no control sequence may rewrite the prompt.
		label := "input " + redact.Printable(id)

		if d := inputs[id].Description; d != "" {
			label = fmt.Sprintf("input %s (%s)", redact.Printable(id), redact.Printable(d))
		}

		v, err := readSecret(in, prompt, label)

		if err != nil {
			return nil, err
		}

		values[id] = v
	}

	var out []pendingSecret

	for _, pc := range plan.Connectors {
		for _, s := range pc.Secrets {
			out = append(out, pendingSecret{name: s.Name, label: s.Label, value: s.Resolve(values)})
		}
	}

	return out, nil
}

// renderImportPlan writes what the import would add and skip. It never
// writes a secret's value.
func renderImportPlan(b *bytes.Buffer, home string, found clientconfig.Found, plan clientconfig.Plan, checkOAuth bool) {
	// Every value shown comes from a file someone else may have written, so
	// none may reach the terminal as a control sequence (wayfindr-awvh).
	p := redact.Printable
	files := make([]string, len(found.Files))

	for i, f := range found.Files {
		files[i] = p(tildePath(home, f))
	}

	if len(files) > 0 {
		fmt.Fprintf(b, "Read %s.\n", strings.Join(files, ", "))
	}

	for _, f := range found.SkippedProject {
		fmt.Fprintf(b, "Not read: %s (in this project; pass --include-project to read it)\n", p(tildePath(home, f)))
	}

	width := 0

	for _, pc := range plan.Connectors {
		width = max(width, len(pc.Connector.Name))
	}

	for _, sk := range plan.Skips {
		width = max(width, len(sk.From.Name))
	}

	indent := strings.Repeat(" ", width+4)

	if len(plan.Connectors) > 0 {
		b.WriteString("\n")
	}

	for _, pc := range plan.Connectors {
		c := pc.Connector
		fmt.Fprintf(b, "  %-*s  %s  (%s, %s)\n", width, p(c.Name), target(c), pc.From.Scope, p(tildePath(home, pc.From.Source)))

		if c.Name != pc.From.Name {
			fmt.Fprintf(b, "%sfrom %q\n", indent, pc.From.Name)
		}

		for _, kv := range []struct {
			kind string
			m    map[string]config.Template
		}{{"env", c.Env}, {"header", c.Headers}} {
			for _, k := range slices.Sorted(maps.Keys(kv.m)) {
				v := kv.m[k]

				if refs, err := v.Refs(); err == nil && len(refs) == 1 && refs[0].Kind == config.RefSecret && string(v) == refs[0].String() {
					fmt.Fprintf(b, "%s%s %s → secret %s\n", indent, kv.kind, p(k), p(refs[0].Name))
				} else {
					fmt.Fprintf(b, "%s%s %s = %s\n", indent, kv.kind, p(k), p(string(v)))
				}
			}
		}

		switch {
		case c.Auth != nil:
			fmt.Fprintf(b, "%sOAuth: authorize after import\n", indent)
		case checkOAuth && mayUseOAuth(c):
			fmt.Fprintf(b, "%sOAuth: asked once you confirm\n", indent)
		}

		for _, n := range pc.Notes {
			fmt.Fprintf(b, "%snote: %s\n", indent, p(n))
		}
	}

	if len(plan.Skips) > 0 {
		b.WriteString("\nSkipped:\n")

		skips := slices.Clone(plan.Skips)
		slices.SortStableFunc(skips, func(a, b clientconfig.Skip) int { return strings.Compare(a.From.Name, b.From.Name) })

		for _, sk := range skips {
			fmt.Fprintf(b, "  %-*s  %s\n", width, p(sk.From.Name), p(sk.Reason))
		}
	}

	b.WriteString("\n")
}

// target is the connector's command line or URL, for display. The URL's
// query is redacted.
func target(c config.NewConnector) string {
	if c.Command == "" {
		s := redact.URL(string(c.URL))

		if c.Transport != "" {
			s += " (" + c.Transport + ")"
		}

		return s
	}

	parts := []string{c.Command}

	for _, a := range c.Args {
		parts = append(parts, string(a))
	}

	return redact.Printable(strings.Join(parts, " "))
}

// writeRemoveHints says how to remove the imported servers from the client,
// so only Wayfindr runs them.
func writeRemoveHints(w io.Writer, client clientconfig.Client, plan clientconfig.Plan) error {
	var b strings.Builder
	b.WriteString("\nTo run them only through Wayfindr, remove them from the client:\n")

	for _, pc := range plan.Connectors {
		fmt.Fprintf(&b, "  %s\n", redact.Printable(clientconfig.RemoveHint(client, pc.From)))
	}

	_, err := io.WriteString(w, b.String())

	return err
}

func countConnectors(n int) string { return count(n, "connector", "connectors") }

// tildePath writes a path under home as ~/….
func tildePath(home, p string) string {
	if home == "" {
		return p
	}

	if rel, err := filepath.Rel(home, p); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}

	return p
}
