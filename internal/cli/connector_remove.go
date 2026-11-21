package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/newfile"
)

// removeFlags are the flags of `connector remove`.
type removeFlags struct {
	dryRun bool
	yes    bool
}

func newConnectorRemoveCmd(host Host) *cobra.Command {
	var f removeFlags
	cmd := &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Remove a connector and delete what it owned",
		Long: `Remove the [connectors.<name>] table, its subtables, and the comments directly
above them from the config file, keeping the rest of it as it is. A running
daemon drops the connector when it reloads the file.

Then delete what the connector owned: its stored OAuth credential, and each
secret it referenced that nothing left in the config references. The daemon is
started for that if it is not running. A secret still in use is kept, and the
output says what uses it.

First the command says what it will remove, delete, and keep, and asks on a
terminal (or takes --yes). --dry-run says it and changes nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runConnectorRemove(cmd, host, args[0], f)
		},
	}

	fl := cmd.Flags()
	fl.BoolVar(&f.dryRun, "dry-run", false, "show what would be removed and deleted; change nothing")
	fl.BoolVarP(&f.yes, "yes", "y", false, "don't ask before removing")

	return cmd
}

func runConnectorRemove(cmd *cobra.Command, host Host, name string, f removeFlags) error {
	ctx, in, out := cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout()
	p, src, err := readConfigFile(host)

	if err != nil {
		return err
	}

	opts := configOptions(host, p)
	next, removed, err := config.RemoveConnector(src, name, opts)

	if err != nil {
		return editError(err)
	}

	// RemoveConnector parsed both; these can only fail if the file is not
	// what it read.
	before, err := config.Parse(src, opts)

	if err != nil {
		return editError(&config.Error{Kind: config.KindUnsafe, Case: config.CaseUnparsable, Connector: name, Err: err})
	}

	after, err := config.Parse(next, opts)

	if err != nil {
		return editError(&config.Error{Kind: config.KindUnsafe, Case: config.CaseUnparsable, Connector: name, Err: err})
	}

	orphans := config.OrphanedSecrets(before, after)
	kept := keptSecrets(after, removed, orphans)

	if f.dryRun || !f.yes {
		home, _ := host.Env.HomeDir() // only to shorten the path

		if err := writeRemovePlan(out, name, tildePath(home, p.ConfigFile), removed.UsesOAuth(), orphans, kept); err != nil {
			return err
		}
	}

	if f.dryRun {
		_, err := fmt.Fprintf(out, "Run again without --dry-run to remove %s.\n", name)

		return err
	}

	if ok, err := confirmChange(in, out, f.yes, "Remove "+name+"? [y/N] ", false, "remove it"); !ok {
		return err
	}

	if err := newfile.Replace(p.ConfigFile, src, next, p.ConfigFile); err != nil {
		return editError(err)
	}

	var c cleanup

	if removed.UsesOAuth() || len(orphans) > 0 {
		d, _, err := startDaemon(ctx, host, p)
		c = cleanUp(ctx, d, err, name, removed.UsesOAuth(), orphans)
	}

	var b strings.Builder
	b.WriteString("Removed " + name + ".")

	if s := c.deleted(); s != "" {
		b.WriteString(" " + s)
	}

	b.WriteString("\n")

	for _, k := range kept {
		fmt.Fprintf(&b, "Kept %s.\n", k)
	}

	if _, err := fmt.Fprint(out, b.String()); err != nil {
		return err
	}

	return c.error("everything " + name + " owned")
}

// keptSecrets says, for each secret the removed connector references that
// is not an orphan, what in after still uses it: "slack_token:
// connectors.slack-2 uses it".
func keptSecrets(after *config.Config, removed config.Connector, orphans []string) []string {
	var kept []string

	for _, s := range removed.Secrets() {
		if slices.Contains(orphans, s) {
			continue
		}

		users := secretUsers(after, s, "", func(c string) string { return "connectors." + c })
		verb := "uses"

		if len(users) > 1 {
			verb = "use"
		}

		kept = append(kept, fmt.Sprintf("%s: %s %s it", s, joinAnd(users), verb))
	}

	return kept
}

// writeRemovePlan says what removing the connector name will do, before it
// is done. Whether a credential or a secret is stored only the daemon
// knows, and the plan is made without it, so each is "if stored".
func writeRemovePlan(w io.Writer, name, configFile string, oauth bool, orphans, kept []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Removing %s will:\n", name)
	fmt.Fprintf(&b, "  remove [connectors.%s] from %s\n", name, configFile)

	if oauth {
		b.WriteString("  delete its OAuth credential, if one is stored\n")
	}

	for _, s := range orphans {
		fmt.Fprintf(&b, "  delete its secret %s, if it is stored\n", s)
	}

	for _, k := range kept {
		fmt.Fprintf(&b, "  keep %s\n", k)
	}

	_, err := io.WriteString(w, b.String())

	return err
}

// cleanup is how deleting what a connector owned went.
type cleanup struct {
	// credential is true when its OAuth credential was deleted.
	credential bool
	// secrets were deleted.
	secrets []string
	// left are the commands that delete what could not be.
	left []string
	// err is the first failure.
	err error
}

// cleanUp deletes the connector's OAuth credential, when revoke is set,
// and secrets through d. A credential or secret the daemon does not have
// is fine: there is nothing to delete. When the daemon could not be
// reached (startErr), nothing is deleted.
func cleanUp(ctx context.Context, d *adminDaemon, startErr error, connector string, revoke bool, secrets []string) cleanup {
	var c cleanup

	fail := func(err error, command string) {
		if c.err == nil {
			c.err = err
		}

		c.left = append(c.left, command)
	}

	if revoke {
		if startErr != nil {
			fail(startErr, "`wayfindr auth logout "+connector+"`")
		} else if found, err := revokeCredential(ctx, d, connector); err != nil {
			fail(err, "`wayfindr auth logout "+connector+"`")
		} else {
			c.credential = found
		}
	}

	for _, s := range secrets {
		if startErr != nil {
			fail(startErr, "`wayfindr secret remove "+s+"`")

			continue
		}

		found, err := deleteSecret(ctx, d, s)

		switch {
		case err != nil:
			fail(err, "`wayfindr secret remove "+s+"`")
		case found:
			c.secrets = append(c.secrets, s)
		}
	}

	return c
}

// deleted says what was deleted, as a sentence about the connector:
// "Deleted its OAuth credential and its secret gmail_client_secret.", or
// nothing.
func (c cleanup) deleted() string {
	var parts []string

	if c.credential {
		parts = append(parts, "its OAuth credential")
	}

	switch len(c.secrets) {
	case 0:
	case 1:
		parts = append(parts, "its secret "+c.secrets[0])
	default:
		parts = append(parts, "its secrets "+strings.Join(c.secrets, ", "))
	}

	if len(parts) == 0 {
		return ""
	}

	return "Deleted " + joinAnd(parts) + "."
}

// error is nil when everything was deleted, and otherwise says why what
// could not be, and the commands that finish the job.
func (c cleanup) error(what string) error {
	if c.err == nil {
		return nil
	}

	return fmt.Errorf("could not delete %s: %w\nTo finish, run %s", what, c.err, joinAnd(c.left))
}

// revokeCredential deletes the connector's stored credential, and reports
// whether there was one.
func revokeCredential(ctx context.Context, d *adminDaemon, connector string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, revokeTimeout)
	defer cancel()

	if _, err := d.Revoke(ctx, connector); err != nil {
		if notFound(err) {
			return false, nil
		}

		return false, d.explain(err)
	}

	return true, nil
}

// deleteSecret deletes the stored secret, and reports whether there was
// one.
func deleteSecret(ctx context.Context, d *adminDaemon, name string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, secretSetTimeout)
	defer cancel()

	if err := d.DeleteSecret(ctx, name); err != nil {
		if notFound(err) {
			return false, nil
		}

		return false, d.explain(err)
	}

	return true, nil
}

// notFound reports whether err is the daemon's 404.
func notFound(err error) bool {
	var ae *admin.APIError

	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// secretUsers names what in cfg references the secret, other than the
// connector skip: each connector, sorted, as label writes it, then
// "embedding".
func secretUsers(cfg *config.Config, secret, skip string, label func(connector string) string) []string {
	var users []string

	for name, c := range cfg.Connectors {
		if name != skip && slices.Contains(c.Secrets(), secret) {
			users = append(users, label(name))
		}
	}

	slices.Sort(users)

	if cfg.Embedding != nil && slices.Contains((&config.Config{Embedding: cfg.Embedding}).SecretRefs(), secret) {
		users = append(users, "embedding")
	}

	return users
}
