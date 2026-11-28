package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
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
	var (
		connectors []string
		remote     bool
		approval   []string
	)

	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a scoped client token and print it once",
		Long: `Create a client token called <name> that can use only the connectors given
with --connector, each of which must be configured. The token is printed once
and cannot be read back. To change its connectors, revoke it and create
another.

With --remote the token may also reach /mcp through a tunnel, which no other
token can. Make one for a cloud agent, and give it only the connectors that
agent needs. A remote token is fixed at creation, like its connectors.

With --approval (repeatable, and only with --remote) some of the token's calls
wait for your approval, which you give with ` + "`wayfindr approvals approve`" + `. An
entry is a class, @write or @destructive, each covering only itself (@write
does not include destructive tools, and a tool with no annotations counts as
destructive); <connector>.<glob>, the tools of that connector whose name
matches; or !<connector>.<glob>, which exempts the tools it matches from the
other entries. The glob is over the upstream's tool name.`,
		Example: `  wayfindr token create nightly --connector github
  wayfindr token create grok-bot --connector slack --connector linear --remote \
    --approval @write --approval @destructive --approval 'linear.run_*'`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The admin API refuses it too; checked here, the flag fails
			// before the daemon is asked.
			if len(approval) > 0 && !remote {
				return errors.New(admin.LocalApprovalMessage) //nolint:staticcheck // ST1005: a full sentence, the words the admin API refuses with
			}

			res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.TokenCreated, error) {
				return d.CreateToken(ctx, admin.TokenSpec{Name: args[0], Connectors: connectors, Remote: remote, Approval: approval})
			})

			if err != nil {
				return err
			}

			kind := ""

			if res.Remote {
				kind = " remote"
			}

			out := cmd.OutOrStdout()

			if _, err := fmt.Fprintf(out, "Created%s client token %s. It can use: %s.\n", kind, res.Name, strings.Join(res.Connectors, ", ")); err != nil {
				return err
			}

			if len(res.Approval) > 0 {
				if _, err := io.WriteString(out, approvalSummary(res.Approval)); err != nil {
					return err
				}
			}

			_, err = fmt.Fprintf(out, "%s\nThis is the only time it is shown; store it where the client can read it.\n", res.Token)

			return err
		},
	}
	cmd.Flags().StringArrayVar(&connectors, "connector", nil, "a connector the token can use (repeatable; at least one)")
	cmd.Flags().BoolVar(&remote, "remote", false, "let the token reach /mcp through a tunnel (cannot be changed later)")
	cmd.Flags().StringArrayVar(&approval, "approval", nil,
		"a call that needs your approval: @write, @destructive, <connector>.<glob>, or !<connector>.<glob> (repeatable; needs --remote)")
	// Registering a flag that exists cannot fail.
	_ = cmd.MarkFlagRequired("connector")

	return cmd
}

func newTokenListCmd(host Host) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List the scoped client tokens and OAuth grants, never their values",
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
		Short: "Delete a scoped client token or an OAuth grant; clients using it are refused",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			res, err := tokenRequest(cmd.Context(), host, func(ctx context.Context, d *adminDaemon) (admin.TokenRevoked, error) {
				return d.RevokeToken(ctx, args[0])
			})

			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "Revoked %s. Clients using it are refused from now on.\n", res.Name)

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

// tokenKind is the KIND column: "token", or "oauth (<client host>)" for an
// OAuth grant, naming the client that holds it.
func tokenKind(t admin.Token) string {
	if t.Kind != admin.TokenKindOAuth {
		return string(admin.TokenKindToken)
	}

	if u, err := url.Parse(t.Client); err == nil && u.Host != "" {
		return fmt.Sprintf("%s (%s)", admin.TokenKindOAuth, u.Host)
	}

	return string(admin.TokenKindOAuth)
}

// writeTokensTable prints one row per scoped token or OAuth grant, by name,
// or says there are none. Times are UTC.
func writeTokensTable(w io.Writer, res admin.TokenList) error {
	var b bytes.Buffer

	if len(res.Tokens) == 0 {
		b.WriteString("No scoped client tokens. Create one with `wayfindr token create <name> --connector <connector>`.\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		// The approval column appears once some token has entries.
		gated := slices.ContainsFunc(res.Tokens, func(t admin.Token) bool { return len(t.Approval) > 0 })
		header := "NAME\tKIND\tCONNECTORS\tREMOTE"

		if gated {
			header += "\tAPPROVAL"
		}

		rows := []string{header + "\tCREATED\tLAST USED"}

		for _, t := range res.Tokens {
			used := "-"

			if t.LastUsedAt != nil {
				used = formatTime(*t.LastUsedAt)
			}

			remote := "no"

			if t.Remote {
				remote = "yes"
			}

			cells := []string{t.Name, tokenKind(t), strings.Join(t.Connectors, ", "), remote}

			if gated {
				cells = append(cells, orDash(strings.Join(t.Approval, " ")))
			}

			rows = append(rows, strings.Join(append(cells, formatTime(t.CreatedAt), used), "\t"))
		}

		// A tabwriter over a bytes.Buffer cannot fail.
		_, _ = io.WriteString(tw, strings.Join(rows, "\n")+"\n")
		_ = tw.Flush()
	}

	_, err := w.Write(b.Bytes())

	return err
}

// approvalSummary is the lines `token create` prints for a token's approval
// entries: what needs the owner's approval, and, when @write stands without
// @destructive, a warning that the riskiest calls are left out. Each line
// ends in a newline. An entry given twice is named once.
func approvalSummary(entries []string) string {
	var classes, names, exempt []string

	seen := make(map[string]bool, len(entries))

	for _, e := range entries {
		if seen[e] {
			continue
		}

		seen[e] = true

		switch rest, negated := strings.CutPrefix(e, "!"); {
		case negated:
			exempt = append(exempt, rest)
		case e == approvalWrite:
			classes = append(classes, "write")
		case e == approvalDestructive:
			classes = append(classes, "destructive")
		default:
			names = append(names, e)
		}
	}

	var what []string

	if len(classes) > 0 {
		what = append(what, strings.Join(classes, " and ")+" tools")
	}

	what = append(what, names...)
	s := "Calls that need your approval: " + strings.Join(what, ", ")

	if len(exempt) > 0 {
		s += ", except " + strings.Join(exempt, ", ")
	}

	s += ".\n"

	if slices.Contains(entries, approvalWrite) && !slices.Contains(entries, approvalDestructive) {
		s += "Warning: " + approvalWrite + " does not cover destructive or unannotated tools. " +
			"Add --approval " + approvalDestructive + " to gate them too.\n"
	}

	return s
}

// The class entries of --approval (config.ParseApproval, whose own
// constants are unexported).
const (
	approvalWrite       = "@write"
	approvalDestructive = "@destructive"
)
