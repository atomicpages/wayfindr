# Quick Start

This page takes you from a fresh install to an agent calling a tool through
Wayfindr. You need `wayfindr` on your PATH ([Installation](./installation)) and
an MCP client, such as Claude Code.

## 1. Set up

```sh
wayfindr init
```

```text
Wayfindr is set up.
…
```

`init` writes a default config, the master key that encrypts your secrets, and
the client token, and never overwrites anything. It also prints the command that
adds Wayfindr to each client; step 3 uses it.

On a terminal, `init` also asks whether to send the maintainers an anonymous
daily [usage report](../reference/telemetry). Press Enter to decline.

You don't have to start the daemon yourself. `wayfindr connector add` starts it
in the background when it isn't running, and so does a client's shim. To run it
at every login instead, see [Running the daemon](./running).

## 2. Add connectors

Add one connector per MCP server. Start with one that needs no auth and one that
uses OAuth:

```sh
wayfindr connector add deepwiki --url https://mcp.deepwiki.com/mcp
wayfindr connector add linear --url https://mcp.linear.app/mcp
```

```text
Checking https://mcp.deepwiki.com/mcp … it needs no auth.
Added connector deepwiki.
deepwiki is ready (3 tools).
Checking https://mcp.linear.app/mcp … it uses OAuth.
Added connector linear.
…
Authorize now? [Y/n] y
…
Authorized linear.
linear is ready (23 tools).
```

Your browser opens the provider's consent page. Approve it, and the command
returns once the token is stored. The `Authorize now?` question appears only in a
terminal; in a script, or to authorize later, run `wayfindr auth login linear`. Tools
are addressed as `<connector>.<tool>`, such as `linear.create_issue`.

::: tip
Already using MCP servers? Import them from your client instead:
`wayfindr connector import claude-code --dry-run`. See
[Importing from a client](./connectors#importing-from-a-client).
:::

<details>
<summary>The same connectors, written by hand</summary>

```toml
# ~/.config/wayfindr/wayfindr.toml

[connectors.deepwiki]               # no auth
url = "https://mcp.deepwiki.com/mcp"

[connectors.linear]                 # OAuth, with discovery and DCR
url  = "https://mcp.linear.app/mcp"
auth = { type = "oauth" }
```

The daemon picks up the change when you save. Then run `wayfindr auth login linear`.

</details>

## 3. Connect your client

::: code-group

```sh [Claude Code]
claude mcp add --scope user --transport stdio wayfindr -- wayfindr stdio
```

```sh [Codex]
codex mcp add wayfindr -- wayfindr stdio
```

```sh [OpenCode]
opencode mcp add --global wayfindr -- wayfindr stdio
```

```sh [Gemini CLI]
gemini mcp add --scope user wayfindr wayfindr stdio
```

```sh [VS Code]
code --add-mcp '{"name":"wayfindr","type":"stdio","command":"wayfindr","args":["stdio"]}'
```

```json [Cursor: ~/.cursor/mcp.json]
{
  "mcpServers": {
    "wayfindr": {
      "type": "stdio",
      "command": "wayfindr",
      "args": ["stdio"]
    }
  }
}
```

:::

Each registers the stdio shim, which forwards the client's requests to the
daemon and starts it if it isn't running. Zed and the rest are in
[Clients](./clients). Then restart your client's session, or start a new one, so
it loads the new server.

## 4. Check it

```sh
wayfindr status
wayfindr search linear issues --size 3
```

```text
CONNECTOR  STATE  TOOLS  LAST SYNC             TOKEN EXPIRES
deepwiki   ready  3      2026-11-07T06:20:17Z  -
linear     ready  23     2026-11-07T06:20:41Z  2026-11-14T06:20:41Z

Embeddings: off, lexical only

RANK  TOOL                 SUMMARY
1     linear.list_issues   List issues in the user's Linear workspace.
2     linear.create_issue  Create a new issue in a Linear team.
3     linear.get_issue     Retrieve an issue by its ID.
More results: run the same command with --page 2
```

Every connector should be `ready`; if one isn't, `status` says what to do under
the table. The `TOKEN EXPIRES` column appears once a connector has a token that
expires. `wayfindr search` shows what your agent's search would return, without
an agent: five results a page by default, and `--size 3` keeps this sample short.
See [Troubleshooting](./troubleshooting).

## 5. Ask your agent

Ask for something one of your connectors can do, such as "Find my open Linear
issues." The agent calls `search_tools` with a query such as "list linear
issues", then runs the tool it picked with `call_tool`. If it doesn't reach for
them, name them: "use wayfindr's search_tools".

## Next steps

- [Connectors](./connectors): stdio servers, API keys, and fixing a connector.
- [Control what agents can use](./access/): tools lists, scopes, and tokens.
- [Running the daemon](./running): run it at login with
  `wayfindr service install`.
- [Monitoring](./monitoring): what agents called, and why search ranks a tool.
