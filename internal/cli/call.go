package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/atomicpages/wayfindr/internal/admin"
	"github.com/atomicpages/wayfindr/internal/config"
	"github.com/atomicpages/wayfindr/internal/redact"
)

const (
	// previewBytes bounds the text `wayfindr call` prints; --output writes
	// the whole result.
	previewBytes = 4 << 10
	// maxRounds bounds the input_required round trips of one call, so an
	// upstream that keeps asking can't hold the command forever.
	maxRounds = 8
)

// callFlags are `wayfindr call`'s flags.
type callFlags struct {
	args, argsFile, tokenFile, output string
	yes                               bool
}

func newCallCmd(host Host) *cobra.Command {
	var f callFlags

	cmd := &cobra.Command{
		Use:   "call <connector.tool>",
		Short: "Run one tool call through the daemon, as your agent would",
		Long: `Run one real call of a connector's tool through the running daemon: the same
checks, narrowing, and call log as an agent's call_tool, as the owner, or as a
client token with --token-file. Use it to prove a sign-in works, or to see what
a token's agent sees.

Arguments are a JSON object, from --args or --args-file (- reads stdin); none
means {}. ` + "`wayfindr connector tools <connector> <tool>`" + ` shows what the tool
takes, and with --json prints its inputSchema.

A read tool runs at once. A write or destructive tool asks first; --yes skips
the question, and off a terminal the command fails without it. When the
upstream asks for input mid-call, its form is asked on stdin, or its URL is
printed to open; with --json the question is printed instead, for the caller
to answer.

The output is the result's text, cut at 4 KiB; --output writes the whole
result as JSON, and --json prints the daemon's full response.

With a --token-file token that needs your approval for the call, the call is
refused as an agent's would be, and the command prints how to approve it:
` + "`wayfindr approvals approve <id>`" + `, then run the same call again.`,
		Example: `  wayfindr connector tools linear create_issue
  wayfindr call linear.create_issue --args-file args.json
  wayfindr call linear.search_issues --args '{"query":"login bug"}'
  wayfindr call github.list_repos --token-file ./nightly.token`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCall(cmd, host, args[0], f)
		},
	}
	cmd.Flags().StringVar(&f.args, "args", "", "the tool's arguments as a JSON object")
	cmd.Flags().StringVar(&f.argsFile, "args-file", "", "read the arguments from a file (- reads stdin)")
	cmd.Flags().StringVar(&f.tokenFile, "token-file", "", "run as the client token in this file (- reads stdin)")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "run a write or destructive tool without asking")
	cmd.Flags().StringVar(&f.output, "output", "", "write the whole result as JSON to this file")
	cmd.MarkFlagsMutuallyExclusive("args", "args-file")
	supportsJSON(cmd)

	return cmd
}

// runCall is `wayfindr call name`.
func runCall(cmd *cobra.Command, host Host, name string, f callFlags) error {
	connector, tool, ok := strings.Cut(name, ".")

	if !ok || connector == "" || tool == "" {
		return fmt.Errorf("name the tool as <connector>.<tool>, such as linear.create_issue, not %q", name)
	}

	if f.argsFile == "-" && f.tokenFile == "-" {
		return errors.New("--args-file and --token-file can't both read stdin")
	}

	ctx := cmd.Context()
	d, err := findDaemon(ctx, host)

	if err != nil {
		return err
	}

	entry, listErr, err := listedTool(ctx, d, connector, tool)

	if err != nil {
		return err
	}

	stdin := cmd.InOrStdin()
	arguments, err := readArguments(stdin, f)

	if err != nil {
		return err
	}

	token, err := readTokenFile(stdin, f.tokenFile)

	if err != nil {
		return err
	}

	// A tool the tools list hides is refused by the daemon, never run, so
	// there is nothing to confirm. A tool whose class is unknown, since the
	// listing failed, is asked about as a destructive one: the call still
	// goes out, so the gateway's own error and the call log show why.
	q := ""

	switch {
	case listErr != nil:
		if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "%s's tools could not be listed: %s\n",
			connector, printableText(listErr.Error())); err != nil {
			return err
		}

		q = fmt.Sprintf("The class of %s is unknown, so it may change or delete data. Run it? [y/N] ", name)
	case entry.Exposed && entry.Class != string(config.ClassRead):
		q = fmt.Sprintf("%s is a %s tool. Run it? [y/N] ", name, entry.Class)
	}

	if q != "" {
		if ok, err := confirmChange(stdin, cmd.ErrOrStderr(), f.yes, q, false, "run it"); !ok {
			return err
		}
	}

	req := admin.CallRequest{Name: name, Arguments: arguments, Client: admin.CallClientCLI, Token: token}
	answers := bufio.NewReader(stdin)
	stdinTaken := f.argsFile == "-" || f.tokenFile == "-"

	for round := 0; ; round++ {
		resp, err := d.Call(ctx, req)

		if err != nil {
			return d.explain(err)
		}

		res, err := resp.Decode()

		if err != nil {
			return fmt.Errorf("read the daemon's answer: %w", err)
		}

		if !res.InputRequired() || wantJSON(cmd) {
			return finishCall(cmd, f, resp, res)
		}

		if round+1 >= maxRounds {
			return fmt.Errorf("%s asked for input %d times; stopping", connector, maxRounds)
		}

		if stdinTaken {
			return errors.New("the tool asks for input, but stdin held the arguments or the token: " +
				"pass them from a file, or use --json and answer yourself")
		}

		responses, err := answerAll(answers, cmd.ErrOrStderr(), res.InputRequests)

		if err != nil {
			return err
		}

		req.RequestState, req.InputResponses = res.RequestState, responses
	}
}

// listedTool is connector's tool as the daemon lists it: its class, for the
// question. The owner's listing shows
// hidden tools too. It is bounded as `connector tools` is; the call itself
// is bounded by the connector's timeout in the daemon.
//
// A listing the daemon refused, such as for a connector that is not ready,
// is listErr: the caller still calls, with the class unknown. A tool the
// connector doesn't serve, and a daemon that can't be reached, are err.
func listedTool(ctx context.Context, d *adminDaemon, connector, tool string) (entry admin.ToolEntry, listErr, err error) {
	listCtx, cancel := context.WithTimeout(ctx, toolsTimeout)
	list, lerr := d.Tools(listCtx, connector)

	cancel()

	if ae := (*admin.APIError)(nil); errors.As(lerr, &ae) {
		return admin.ToolEntry{}, lerr, nil
	}

	if lerr != nil {
		return admin.ToolEntry{}, nil, d.explain(lerr)
	}

	i := slices.IndexFunc(list.Tools, func(t admin.ToolEntry) bool { return t.Name == tool })

	if i < 0 {
		return admin.ToolEntry{}, nil, fmt.Errorf("%s serves no tool called %s: list them with `wayfindr connector tools %s`",
			connector, redact.Printable(tool), connector)
	}

	return list.Tools[i], nil, nil
}

// readArguments is the call's arguments from --args or --args-file: a JSON
// object, or {} when neither is given.
func readArguments(stdin io.Reader, f callFlags) (json.RawMessage, error) {
	raw := []byte(f.args)
	from := "--args"

	switch {
	case f.argsFile == "-":
		b, err := io.ReadAll(io.LimitReader(stdin, admin.MaxCallBody+1))

		if err != nil {
			return nil, fmt.Errorf("read the arguments from stdin: %w", err)
		}

		raw, from = b, "stdin"
	case f.argsFile != "":
		b, err := readCapped(f.argsFile, admin.MaxCallBody+1)

		if err != nil {
			return nil, fmt.Errorf("read the arguments: %w", err)
		}

		raw, from = b, f.argsFile
	case f.args == "":
		return json.RawMessage("{}"), nil
	}

	raw = bytes.TrimSpace(raw)

	if !json.Valid(raw) || len(raw) == 0 || raw[0] != '{' {
		return nil, fmt.Errorf("the arguments in %s are not a JSON object; `wayfindr connector tools <connector> <tool>` shows what it takes", from)
	}

	return raw, nil
}

// readTokenFile is the client token in path (- reads stdin), trimmed, or
// "" for no path. The value is never a command-line argument, where ps and
// shell history would see it.
func readTokenFile(stdin io.Reader, path string) (string, error) {
	var (
		b   []byte
		err error
	)

	switch path {
	case "":
		return "", nil
	case "-":
		b, err = io.ReadAll(io.LimitReader(stdin, maxTokenFile))
	default:
		b, err = readCapped(path, maxTokenFile)
	}

	if err != nil {
		return "", fmt.Errorf("read the client token: %w", err)
	}

	token := strings.TrimSpace(string(b))

	if token == "" {
		return "", errors.New("the token file is empty")
	}

	return token, nil
}

// maxTokenFile bounds what --token-file reads: a token is far shorter.
const maxTokenFile = 4 << 10

// readCapped reads at most limit bytes of the file at path, as stdin is
// read: a larger file is cut, and the daemon refuses the cut body.
func readCapped(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the file the user named with a flag

	if err != nil {
		return nil, err
	}

	defer func() { _ = f.Close() }()

	return io.ReadAll(io.LimitReader(f, limit))
}

// finishCall prints the call's outcome: the full response with --json, else
// a status line and the text preview, and writes --output. An isError
// result exits 1; an input_required one (--json only) exits 0.
func finishCall(cmd *cobra.Command, f callFlags, resp admin.CallResponse, res admin.ToolResult) error {
	if f.output != "" {
		var b bytes.Buffer

		if err := json.Indent(&b, resp.Result, "", "  "); err != nil {
			return fmt.Errorf("write %s: %w", f.output, err)
		}

		b.WriteByte('\n')

		if err := os.WriteFile(f.output, b.Bytes(), 0o600); err != nil {
			return fmt.Errorf("write the result: %w", err)
		}
	}

	if wantJSON(cmd) {
		if err := writeJSON(cmd.OutOrStdout(), resp); err != nil {
			return err
		}
	} else {
		if err := writeCallResult(cmd.OutOrStdout(), resp, res, f.output); err != nil {
			return err
		}

		// The gateway sets Approval only for a call refused as pending, so
		// the command is never offered for a denied or expired one, and no
		// refusal text is read for it.
		if resp.Approval != "" {
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "To approve it: wayfindr approvals approve %s\n", redact.Printable(resp.Approval)); err != nil {
				return err
			}
		}
	}

	if res.IsError {
		return exitWith(cmd, 1)
	}

	return nil
}

// writeCallResult prints the status line ("ok · 1.4s · 231 bytes") and the
// result's content: text through redact.Printable, cut at previewBytes,
// and other content as one line each.
func writeCallResult(w io.Writer, resp admin.CallResponse, res admin.ToolResult, output string) error {
	var b bytes.Buffer

	status := "ok"

	if res.IsError {
		status = "error"
	}

	fmt.Fprintf(&b, "%s · %.1fs · %s\n", status, float64(resp.ElapsedMS)/1000, admin.FormatBytes(int64(len(resp.Result))))

	var text strings.Builder

	for _, c := range res.Content {
		if text.Len() > 0 {
			text.WriteByte('\n')
		}

		text.WriteString(c.Summary())
	}

	if text.Len() == 0 && len(res.StructuredContent) > 0 {
		text.Write(res.StructuredContent)
	}

	preview := text.String()

	if len(preview) > previewBytes {
		preview = cutRunes(preview, previewBytes)
		more := "pass --output <file> for the whole result"

		if output != "" {
			more = "the whole result is in " + output
		}

		preview += fmt.Sprintf("\n… cut at 4 KiB of %s; %s.", admin.FormatBytes(int64(text.Len())), more)
	}

	if preview != "" {
		b.WriteString(printableText(preview))
		b.WriteByte('\n')
	}

	_, err := w.Write(b.Bytes())

	return err
}

// printableText is s made safe for a terminal, line by line: tabs become
// spaces, and every other control character is replaced
// (redact.Printable), so upstream text can't move the cursor or reorder a
// line.
func printableText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")

	for i, l := range lines {
		lines[i] = redact.Printable(strings.ReplaceAll(l, "\t", "    "))
	}

	return strings.Join(lines, "\n")
}

// cutRunes is s cut to at most n bytes on a rune boundary.
func cutRunes(s string, n int) string {
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}

	return s[:n]
}
