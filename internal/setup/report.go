package setup

import (
	"fmt"
	"io"
	"strings"

	"github.com/atomicpages/wayfindr/internal/secrets"
)

// Snippet is one harness's config for reaching Wayfindr.
type Snippet struct {
	// Title names the harness and what to do with Body: run it, or add it to
	// the named config file.
	Title string
	Body  string
}

// Snippets returns how to connect each supported harness to Wayfindr through
// the stdio shim: a command where the harness has its own "mcp add", else the
// entry to add to its config file. HTTP setup is in the Clients guide.
//
// No snippet holds the client token (design §9): the shim reads it itself.
// Formats, as documented on 2026-11-08:
//
//   - Claude Code: https://code.claude.com/docs/en/mcp
//   - Codex: https://learn.chatgpt.com/docs/extend/mcp?surface=cli
//   - OpenCode: https://opencode.ai/docs/mcp-servers/ — flags before the name.
//   - Hermes Agent: https://hermes-agent.nousresearch.com/docs/user-guide/features/mcp
//   - Gemini CLI: https://geminicli.com/docs/tools/mcp-server/ — the
//     default scope is the project.
//   - GitHub Copilot CLI: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers
//   - VS Code: https://code.visualstudio.com/docs/agents/reference/mcp-configuration
//   - Cursor: https://cursor.com/docs/context/mcp — "type": "stdio" for a
//     command entry.
//   - Zed: https://zed.dev/docs/ai/mcp — the key is context_servers.
//   - Windsurf (Devin Desktop): https://docs.devin.ai/windsurf/plugins/cascade/mcp
//   - goose: https://github.com/aaif-goose/goose/blob/main/documentation/docs/guides/config-files.md
func Snippets() []Snippet {
	return []Snippet{
		{Title: "Claude Code: run", Body: "  claude mcp add --scope user --transport stdio wayfindr -- wayfindr stdio"},
		{Title: "Codex: run", Body: "  codex mcp add wayfindr -- wayfindr stdio"},
		{Title: "OpenCode: run", Body: "  opencode mcp add --global wayfindr -- wayfindr stdio"},
		{Title: "Hermes Agent: run", Body: "  hermes mcp add wayfindr --command wayfindr --args stdio"},
		{Title: "Gemini CLI: run", Body: "  gemini mcp add --scope user wayfindr wayfindr stdio"},
		{Title: "GitHub Copilot CLI: run", Body: "  copilot mcp add wayfindr -- wayfindr stdio"},
		{Title: "VS Code: run", Body: `  code --add-mcp '{"name":"wayfindr","type":"stdio","command":"wayfindr","args":["stdio"]}'`},
		{Title: "Cursor: add to ~/.cursor/mcp.json", Body: `{
  "mcpServers": {
    "wayfindr": { "type": "stdio", "command": "wayfindr", "args": ["stdio"] }
  }
}`},
		{Title: "Zed: add to settings.json", Body: `{
  "context_servers": {
    "wayfindr": { "command": "wayfindr", "args": ["stdio"] }
  }
}`},
		{Title: "Windsurf (Devin Desktop): add to ~/.config/devin/mcp_config.json", Body: `{
  "mcpServers": {
    "wayfindr": { "command": "wayfindr", "args": ["stdio"] }
  }
}`},
		{Title: "goose: add to ~/.config/goose/config.yaml", Body: `extensions:
  wayfindr:
    type: stdio
    name: wayfindr
    cmd: wayfindr
    args: [stdio]
    enabled: true
    timeout: 300`},
	}
}

// clientsGuideURL is the docs page with every harness's HTTP and stdio setup.
const clientsGuideURL = "https://atomicpages.github.io/wayfindr/guide/clients"

// telemetryURL is the docs page listing what the usage report holds.
const telemetryURL = "https://atomicpages.github.io/wayfindr/reference/telemetry"

// WriteReport prints what Init found or created, then the harness snippets.
// It never prints the client token or the master key.
func WriteReport(w io.Writer, r Result) error {
	var b strings.Builder
	b.WriteString("Wayfindr is set up.\n\n")
	fmt.Fprintf(&b, "  Config        %s (%s)\n", r.Paths.ConfigFile, createdOrKept(r.ConfigCreated))
	fmt.Fprintf(&b, "  Data dir      %s\n", r.Paths.DataDir)
	fmt.Fprintf(&b, "  Master key    %s\n", keySourceLabel(r))
	fmt.Fprintf(&b, "  Client token  %s (%s)\n", r.Paths.ClientTokenFile(), createdOrKept(r.TokenCreated))

	if r.TokenFromEnv {
		fmt.Fprintf(&b, "                %s is set and takes precedence over this file.\n", EnvClientToken)
	}

	b.WriteString(`
Add a connector
---------------

  wayfindr connector add linear --url https://mcp.linear.app/mcp
  wayfindr connector add fs -- npx -y @modelcontextprotocol/server-filesystem ~/src

The command asks an HTTP server whether it uses OAuth and offers to
authorize it. wayfindr status shows every connector's state.
`)

	if len(r.Importable) > 0 {
		b.WriteString("\nTo bring over the MCP servers your clients already have, see what\nimporting them would add:\n\n")

		for _, c := range r.Importable {
			fmt.Fprintf(&b, "  wayfindr connector import %s --dry-run\n", c)
		}
	}

	b.WriteString(`
Connect a client
----------------

Add Wayfindr to your harness with the stdio shim (wayfindr stdio). The shim
reads the client token itself and starts the daemon if it is not running.
It needs wayfindr on the harness's PATH.
`)

	for _, s := range Snippets() {
		fmt.Fprintf(&b, "\n%s\n%s\n", s.Title, s.Body)
	}

	fmt.Fprintf(&b, `
To connect over HTTP instead (http://%[1]s/mcp), start the daemon
(wayfindr start) and set the client token where the harness runs:

  export %[2]s="$(cat %[3]s)"

HTTP setup for each harness: %[4]s

`, r.Listen, EnvClientToken, shellQuote(r.Paths.ClientTokenFile()), clientsGuideURL)

	if r.TelemetryEnabled {
		fmt.Fprintf(&b, `Usage telemetry is on: the daemon sends the maintainers an anonymous daily
usage report. To stop, set enabled = false under [telemetry] in the config.
What it holds: %s
`, telemetryURL)
	} else {
		fmt.Fprintf(&b, `Usage telemetry is off by default. To send the maintainers an anonymous
daily usage report, set enabled = true under [telemetry] in the config.
What it holds: %s
`, telemetryURL)
	}

	if _, err := io.WriteString(w, b.String()); err != nil {
		return fmt.Errorf("write init report: %w", err)
	}

	return nil
}

func createdOrKept(created bool) string {
	if created {
		return "created"
	}

	return "kept"
}

func keySourceLabel(r Result) string {
	switch r.KeySource {
	case secrets.SourceEnv:
		return secrets.EnvSecretKey
	case secrets.SourceKeychain:
		return "OS keychain"
	case secrets.SourceFile:
		return "key file " + r.Paths.KeyFile()
	default:
		return string(r.KeySource)
	}
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
