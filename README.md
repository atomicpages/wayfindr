<h1 align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/public/logo-dark.svg">
    <img src="docs/public/logo.svg" alt="Wayfindr" width="420">
  </picture>
</h1>

Connect all your MCP servers through one gateway.

Wayfindr is a local daemon that holds the credentials for every MCP server you
use, including OAuth for servers that don't support Dynamic Client
Registration. It exposes them to your coding agent through just two tools:

- `search_tools` finds relevant tools by natural language or fuzzy match.
- `call_tool` calls one of them.

Your agent's context gets two tool definitions instead of hundreds. Connector
auth lives in one place instead of in each harness.

## Features

- **Every kind of MCP server:** stdio, Streamable HTTP, and legacy HTTP+SSE,
  at old and new protocol versions.
- **Credentials in one place:** OAuth (discovery and DCR, your own client, or
  CIMD), API keys, and encrypted secrets. Tokens refresh on demand and never
  reach your agent.
- **Import:** move the MCP servers Claude Code, Cursor, Codex, or VS Code
  already runs into Wayfindr with one command.
- **Control what agents reach:** tools lists hide tools, project scopes and
  profiles narrow what a project or client gets, and scoped client tokens
  hold an unattended agent to some connectors.
- **Questions from tools:** a tool that asks you something mid-call
  (elicitation) reaches your client.
- **Call log:** see every tool call, which client made it, and how it ended,
  without its arguments or results.
- **Hybrid search:** fuzzy and full-text matching, plus optional embeddings
  from any OpenAI-compatible endpoint, such as a local Ollama.
- **One static binary:** one TOML config that reloads when you save it.
  SQLite by default, Postgres with pgvector if you want it.

## Install

On macOS or Linux:

```bash
curl -fsSL https://atomicpages.github.io/wayfindr/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://atomicpages.github.io/wayfindr/install.ps1 | iex
```

The installer checks the download against the release's `checksums.txt`.
See [Installation](https://atomicpages.github.io/wayfindr/guide/installation)
for pinning a version, verifying build provenance, and building from source.

## Get started

```bash
wayfindr init                    # config, master key, store, and client token
wayfindr start                   # run the daemon in the background
wayfindr connector add linear --url https://mcp.linear.app/mcp
wayfindr status                  # each connector's state and what to fix
```

`connector add` asks the server whether it uses OAuth and offers to
authorize it. Already have MCP servers set up in a client? Run
`wayfindr connector import claude-code --dry-run` to see what it would add,
then run it again without `--dry-run`. Then connect your agent, for example
Claude Code:

```bash
claude mcp add --scope user --transport stdio wayfindr -- wayfindr stdio
```

`wayfindr init` prints the line for every supported harness.

## Documentation

The full documentation is at
[atomicpages.github.io/wayfindr](https://atomicpages.github.io/wayfindr/):

- [Quick Start](https://atomicpages.github.io/wayfindr/guide/quick-start)
- [Clients](https://atomicpages.github.io/wayfindr/guide/clients): setup for
  Claude Code, Codex, Cursor, and others, over stdio or HTTP
- [Connectors](https://atomicpages.github.io/wayfindr/guide/connectors) and
  [Authentication](https://atomicpages.github.io/wayfindr/guide/authentication)
- [Control what agents can use](https://atomicpages.github.io/wayfindr/guide/access/)
- [Troubleshooting](https://atomicpages.github.io/wayfindr/guide/troubleshooting)
- [CLI reference](https://atomicpages.github.io/wayfindr/reference/cli) and
  [configuration reference](https://atomicpages.github.io/wayfindr/reference/configuration)

## Contributing

- The [documentation site](https://atomicpages.github.io/wayfindr/) describes
  what Wayfindr does; the code is the final word.
- [docs/internal/design.md](docs/internal/design.md): the technical design.
- [docs/adr/](docs/adr/): architecture decisions.
- [GLOSSARY.md](GLOSSARY.md): domain terms.
- [docs/internal/compat.md](docs/internal/compat.md): harness compatibility and
  conformance.

```bash
make check          # lint, test, race, build; run before sending changes
make conformance    # MCP conformance suite (needs Node.js)
make test-postgres  # Postgres store tests (needs Docker)
```

Read [CODING_STANDARDS.md](CODING_STANDARDS.md) before writing code.

## License

[Apache 2.0](LICENSE)
