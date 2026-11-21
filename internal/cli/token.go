package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
)

// tokenTimeout bounds a token request; the daemon answers from the store.
const tokenTimeout = 10 * time.Second

// tokenRequest finds the daemon and makes one token request of it with call,
// both within tokenTimeout. A failed request is explained (adminDaemon.explain).
func tokenRequest[T any](ctx context.Context, host Host, call func(context.Context, *adminDaemon) (T, error)) (T, error) {
	var zero T

	ctx, cancel := context.WithTimeout(ctx, tokenTimeout)
	defer cancel()
	d, err := findDaemon(ctx, host)

	if err != nil {
		return zero, err
	}

	res, err := call(ctx, d)

	if err != nil {
		return zero, d.explain(err)
	}

	return res, nil
}

func newTokenCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "token",
		Short: "Manage scoped client tokens, each limited to some connectors",
		Long: `A scoped client token lets a client use only the connectors it names, and
never the admin API. Give one to an unattended agent instead of the default
client token. A client presents it as it would the default one.`,
		Args: cobra.ArbitraryArgs,
		RunE: runGroup,
	}
	cmd.AddCommand(newTokenCreateCmd(host), newTokenListCmd(host), newTokenRevokeCmd(host))

	return cmd
}

func newTokenCreateCmd(host Host) *cobra.Command {
	var connectors []string
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a scoped client token and print it once",
		Long: `Create a client token called <name> that can use only the connectors given
with --connector, each of which must be configured. The token is printed once
and cannot be read back. To change its connectors, revoke it and create
another.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.TokenCreated, error) {
				return d.CreateToken(ctx, args[0], connectors)
			})

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Created client token %s. It can use: %s.\n%s\nThis is the only time it is shown; store it where the client can read it.\n",
				res.Name, strings.Join(res.Connectors, ", "), res.Token)

			return err
		},
	}
	cmd.Flags().StringArrayVar(&connectors, "connector", nil, "a connector the token can use (repeatable; at least one)")
	// Registering a flag that exists cannot fail.
	_ = cmd.MarkFlagRequired("connector")

	return cmd
}

func newTokenListCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the scoped client tokens, never their values",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.TokenList, error) {
				return d.ListTokens(ctx)
			})

			if err != nil {
				return err
			}

			if wantJSON(cmd) {
				return writeTokensJSON(cmd.OutOrStdout(), res)
			}

			return writeTokensTable(cmd.OutOrStdout(), res)
		},
	}
	supportsJSON(cmd)

	return cmd
}

func newTokenRevokeCmd(host Host) *cobra.Command {
	return &cobra.Command{
		Use:   "revoke <name>",
		Short: "Delete a scoped client token; clients using it are refused",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.TokenRevoked, error) {
				return d.RevokeToken(ctx, args[0])
			})

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Revoked client token %s. Clients using it are refused from now on.\n", res.Name)

			return err
		},
	}
}

func writeTokensJSON(w io.Writer, res admin.TokenList) error {
	b, err := json.MarshalIndent(res, "", "  ")

	if err != nil {
		return err
	}

	_, err = w.Write(append(b, '\n'))

	return err
}

// writeTokensTable prints one row per scoped token, by name, or says there
// are none. Times are UTC.
func writeTokensTable(w io.Writer, res admin.TokenList) error {
	var b bytes.Buffer

	if len(res.Tokens) == 0 {
		b.WriteString("No scoped client tokens. Create one with `wayfindr token create <name> --connector <connector>`.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		rows := []string{"NAME\tCONNECTORS\tCREATED\tLAST USED"}

		for _, t := range res.Tokens {
			used := "-"

			if t.LastUsedAt != nil {
				used = formatTime(*t.LastUsedAt)
			}

			rows = append(rows, strings.Join([]string{t.Name, strings.Join(t.Connectors, ", "), formatTime(t.CreatedAt), used}, "\t"))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	_, err := w.Write(b.Bytes())

	return err
}
