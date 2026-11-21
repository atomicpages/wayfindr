package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
)

// secretSetTimeout bounds the request. The daemon reconnects the connectors
// waiting on the secret before it answers, in parallel, each within the
// 60s connect timeout.
const secretSetTimeout = 90 * time.Second

// secretListTimeout bounds the list request; the daemon answers from
// memory and the store.
const secretListTimeout = 10 * time.Second

func newSecretCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secret",
		Short: "Manage the secrets ${secret:name} references resolve to",
		Args:  cobra.ArbitraryArgs,
		RunE:  runGroup,
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "set <name>",
		Short: "Store a secret's value, read from stdin",
		Long: `Read a secret's value from stdin and store it, sealed, in the daemon.
On a terminal the value is prompted for without echo; when piped, one trailing
newline is dropped. Connectors whose config refers to ${secret:<name>} pick it
up on their next connect; the ones waiting on it reconnect now.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			// Refuse here what no secret can be called, before the prompt.
			if err := config.CheckSecretName(name, true); err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), secretSetTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			// A name only the older rule allows is fine for a known secret
			// alone; find out before asking for a value the daemon would
			// refuse. The daemon still decides.
			if !config.IsSecretName(name) {
				known, err := secretKnown(ctx, d, host, name)

				if err != nil {
					return err
				}

				if err := config.CheckSecretName(name, known); err != nil {
					return err
				}
			}

			value, err := readSecret(cmd.InOrStdin(), cmd.ErrOrStderr(), "secret "+name)

			if err != nil {
				return err
			}

			res, err := d.PutSecret(ctx, name, value)

			if err != nil {
				return d.explain(err)
			}

			return writeSecretSet(cmd.OutOrStdout(), res)
		},
	}, newSecretListCmd(host), &cobra.Command{
		Use:     "remove <name>",
		Aliases: []string{"rm"},
		Short:   "Delete a stored secret",
		Long: `Delete a stored secret. A connector whose config still refers to
${secret:<name>} goes to auth_error when it next needs the value, so use it for
a secret nothing references; "wayfindr connector remove" and "wayfindr
connector edit" delete the secrets they leave unused.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]

			if err := config.CheckSecretName(name, true); err != nil {
				return err
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), secretSetTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			if err := d.DeleteSecret(ctx, name); err != nil {
				return d.explain(err)
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Deleted secret %s.\n", name)

			return err
		},
	})

	return cmd
}

func newSecretListCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List stored secrets and what refers to each, never their values",
		Long: `List the names of the secrets stored in the daemon, sorted, and what in its
running config refers to each: a connector (connectors.<name>) or
[embedding] api_key (embedding.api_key), or "-" when nothing does. Values
are never printed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), secretListTimeout)
			defer cancel()
			d, err := findDaemon(ctx, host)

			if err != nil {
				return err
			}

			res, err := d.ListSecrets(ctx)

			if err != nil {
				return d.explain(err)
			}

			if wantJSON(cmd) {
				return writeSecretListJSON(cmd.OutOrStdout(), res)
			}

			return writeSecretList(cmd.OutOrStdout(), res)
		},
	}
	supportsJSON(cmd)

	return cmd
}

func writeSecretListJSON(w io.Writer, res admin.SecretList) error {
	b, err := json.MarshalIndent(res, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}

// writeSecretList prints one row per stored secret, with what refers to it
// or "-", or says there are none.
func writeSecretList(w io.Writer, res admin.SecretList) error {
	var b bytes.Buffer

	if len(res.Secrets) == 0 {
		b.WriteString("No secrets are stored.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"NAME\tUSED BY"}

		for _, s := range res.Secrets {
			rows = append(rows, s.Name+"\t"+orDash(strings.Join(s.UsedBy, ", ")))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	_, err := w.Write(b.Bytes())

	return err
}

// readSecret reads a secret's value from in: without echo, after a prompt
// for label (such as "secret tok") on prompt, when in is a terminal;
// otherwise all of it, less one trailing newline. It refuses an empty
// value and one whose request body would be over the daemon's limit.
func readSecret(in io.Reader, prompt io.Writer, label string) (string, error) {
	var value string

	if f, ok := terminal(in); ok {
		// The prompt is a courtesy; failing to write it is not an error.
		_, _ = fmt.Fprintf(prompt, "Value for %s: ", label)
		b, err := term.ReadPassword(int(f.Fd()))
		_, _ = fmt.Fprintln(prompt)

		if err != nil {
			return "", fmt.Errorf("read secret value: %w", err)
		}

		value = string(b)
	} else {
		b, err := io.ReadAll(io.LimitReader(in, admin.MaxSecretBody+1))

		if err != nil {
			return "", fmt.Errorf("read secret value: %w", err)
		}

		value = string(b)

		if s, ok := strings.CutSuffix(value, "\n"); ok {
			value = strings.TrimSuffix(s, "\r")
		}
	}

	if value == "" {
		return "", errors.New("secret value is empty")
	}

	// The daemon limits the request body, not the value: check the body
	// PutSecret will send, wrapper and escaping included.
	if n := admin.SecretBodySize(value); n > admin.MaxSecretBody {
		return "", fmt.Errorf("secret value is too large: sent as JSON it is %d bytes, over the daemon's %d-byte limit", n, admin.MaxSecretBody)
	}

	return value, nil
}

// terminal returns in as a file when it is a terminal.
func terminal(in io.Reader) (*os.File, bool) {
	f, ok := in.(*os.File)

	return f, ok && term.IsTerminal(int(f.Fd()))
}

// writeSecretSet confirms the set and says how each connector waiting on
// the secret came out: ready, or upstream's advice for its new problem.
func writeSecretSet(w io.Writer, res admin.SecretSet) error {
	var b bytes.Buffer

	if len(res.Retried) == 0 {
		fmt.Fprintf(&b, "Secret %s set. Connectors that use it pick it up on their next connect.\n", res.Name)
	} else {
		fmt.Fprintf(&b, "Secret %s set.\n", res.Name)
	}

	for _, c := range res.Retried {
		renderConnector(&b, c)
	}

	_, err := w.Write(b.Bytes())

	return err
}

// secretKnown reports whether name is a stored secret, or one the config
// file refers to, the two ways a name the older rule allows stays valid
// (config.CheckSecretName). The file stands in for the running config,
// which is the file unless an edit since failed to load; a file that
// doesn't load counts as referring to nothing.
func secretKnown(ctx context.Context, d *adminDaemon, host Host, name string) (bool, error) {
	list, err := d.ListSecrets(ctx)

	if err != nil {
		return false, d.explain(err)
	}

	for _, s := range list.Secrets {
		if s.Name == name {
			return true, nil
		}
	}

	p, src, err := readConfigFile(host)

	if err != nil {
		return false, nil //nolint:nilerr // no file refers to nothing; the daemon reports a real problem.
	}

	cfg, err := config.Parse(src, configOptions(host, p))

	if err != nil {
		return false, nil //nolint:nilerr // an invalid file refers to nothing; status shows why.
	}

	_, ok := cfg.SecretUsers()[name]

	return ok, nil
}
