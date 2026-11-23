# Introduction

Wayfindr is an MCP gateway. One local daemon holds the credentials for every MCP
server you use and exposes all of them to your coding agent through two tools:

```text
search_tools  finds relevant tools by natural language or fuzzy match
call_tool     calls one of them by its qualified name, such as linear.create_issue
```

Your agent's context gets two tool definitions instead of hundreds. Connector
auth lives in one place instead of in each harness.

## The problem

Connect a dozen MCP servers to Claude Code, Codex, or Cursor and three things go
wrong.

- **Context.** Every server's tools are listed to the agent up front. A few
  servers add up to hundreds of tool definitions, sent with every request,
  before the agent has done anything.
- **Auth.** Each harness runs its own OAuth flow for each server and stores its
  own tokens. Servers that don't support Dynamic Client Registration often can't
  be used at all, or need a harness-specific plugin. Switch harnesses and you
  authorize everything again.
- **Reconnecting.** Tokens expire, and a harness that can't refresh one stops
  you mid-task to sign in again. Wayfindr refreshes tokens when a call needs it,
  and warns you ahead of the ones it can't refresh.

## How Wayfindr fixes it

You list your MCP servers once, as connectors in
`~/.config/wayfindr/wayfindr.toml`. The daemon connects to each one, keeps a
catalog of their tools, and handles their credentials: OAuth tokens, API keys,
or none. Your clients connect to the daemon, not to the servers.

```text
  Claude Code ─┐                       ┌─► linear   (OAuth, Streamable HTTP)
  Codex       ─┼─► Wayfindr daemon ────┼─► slack    (OAuth, your own app)
  Cursor      ─┘   127.0.0.1:7777      ├─► exa      (API key in a header)
                   two tools:          └─► fs       (stdio, a local process)
                   search_tools
                   call_tool
```

When the agent needs a tool, it asks `search_tools`, gets back a few full tool
definitions, and calls one with `call_tool`. Search runs against the daemon's
catalog, never against the upstreams, so it doesn't wait on them. If a connector
needs you to sign in, the result says so and includes an auth link that starts
the sign-in in your browser.

## Terms

These words have one meaning throughout the docs.

| Term      | Meaning                                                                                                                               |
| --------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| Daemon    | The one long-running Wayfindr process. It owns every connection, credential, and the catalog. See [Running the daemon](./running).    |
| Client    | Anything that connects to Wayfindr over MCP: Claude Code, Codex, Cursor, and other harnesses. See [Clients](./clients).               |
| Connector | One configured upstream MCP server: a `[connectors.<name>]` table in the config. See [Connectors](./connectors).                      |
| Catalog   | The daemon's stored copy of every connector's tool definitions. Search runs against it. See [How search works](./search#the-catalog). |
| Shim      | `wayfindr stdio`: forwards a stdio client to the daemon and starts the daemon if it isn't running. See [Clients](./clients).          |

The upstream is the daemon's live connection to a connector. Other terms, such
as the auth link, client token, and tools list, are defined where they first
matter.

## What it is not

- **Not a proxy for everything in MCP.** Wayfindr proxies tools, and the
  questions a tool asks you during a call (elicitation). Resources, prompts,
  sampling, and roots are not passed through.
- **Not a hosted or shared service.** The daemon runs on your machine, listens
  on loopback only, and serves one user. There is no team or multi-user mode
  yet. A cloud agent such as Grok Bot or Muse can still use it, but only through
  a tunnel you run, with a token you made for it, and only to `/mcp`. See
  [Cloud agents](./cloud-agents).
- **Not a per-tool prompt in your harness.** Your harness sees only `call_tool`,
  so its "allow this tool" prompts cover every upstream tool at once. Choose
  which tools agents get in Wayfindr instead, with a
  [tools list](./access/tools-lists). Upstream annotations, such as
  `destructiveHint`, are passed through in search results.
- **Not a full audit trail.** The [call log](./monitoring#what-did-agents-call)
  records that each call happened and how it ended, never its arguments or
  results.

## Next steps

- [Install Wayfindr](./installation).
- Follow the [quick start](./quick-start) to connect your first server.
- [Control what agents can use](./access/) once you have connectors.
- Read [how it works](/architecture/how-it-works) for the design behind it.
