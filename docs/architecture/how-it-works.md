# How It Works

```
 Claude Code ─┐                       ┌─ linear   (Streamable HTTP, OAuth)
 Codex ───────┼─ stdio shim ─┐        ├─ slack    (Streamable HTTP, OAuth)
 Cursor ──────┘              ▼        ├─ exa      (HTTP, API key)
 any MCP client ─ HTTP ─► Wayfindr ───┼─ fs       (stdio)
                         daemon       └─ legacy   (HTTP+SSE)
                           │
                         store: catalog, embeddings,
                         encrypted secrets and tokens
```

Wayfindr is one daemon that sits between your MCP clients and every MCP server
you use. Clients see two tools. The daemon holds everything else: the
connections, the credentials, and a searchable copy of every tool.

This page explains the moving parts. The [decision records](/adr/) explain why
each part is built the way it is.

## One daemon

`wayfindr start` runs the daemon. It listens on loopback only (`127.0.0.1:7777`
by default) and serves MCP over Streamable HTTP at `/mcp`. There is one daemon
per data directory. A lock file makes sure a second one refuses to start.

Clients that only speak stdio use the shim, `wayfindr stdio`. The shim holds no
state: it forwards each request to the daemon, and it starts the daemon if it
isn't running. Several harnesses can share one daemon this way. That matters for
OAuth: if each harness ran its own gateway, they would race to refresh the same
tokens, and with rotating refresh tokens that race logs you out. See
[ADR 0003](/adr/0003-single-daemon).

Every client presents a client token as a bearer token: the default one that
`wayfindr init` created, or a scoped one limited to some connectors. The shim
reads it for you. See [ADR 0012](/adr/0012-scoped-client-tokens).

## Connectors and upstreams

Each `[connectors.<name>]` table in `wayfindr.toml` is a **connector**: one MCP
server and how to reach it. The daemon's live connection to it is the
**upstream**.

- A connector with `command` is a **stdio** server. The daemon starts it on
  demand and stops it after 10 minutes idle.
- A connector with `url` speaks **Streamable HTTP**. If the server answers the
  Streamable HTTP `initialize` request with a 4xx other than 401 or 403, the
  daemon falls back to legacy **HTTP+SSE**. `transport = "sse"` forces SSE.

Each side negotiates its own MCP version. A client on 2026-07-28 can call a tool
on a server that only speaks 2025-06-18: the daemon keeps the server's session
and bridges the two.

Wayfindr proxies tools, and the questions a tool asks the user during a call
(elicitation), which it passes on to the client. It doesn't proxy resources,
prompts, sampling, or roots. See [ADR 0008](/adr/0008-relay-elicitation).

## The catalog and search

When the daemon connects to a connector, it lists the connector's tools and
stores them in the **catalog**. This is a **sync**. Syncs happen at startup,
when the config or a credential changes, when a server says its tool list
changed, and on a schedule; [The catalog](/guide/search#the-catalog) lists every
trigger.

`search_tools` searches the catalog, never the live servers, so a search is fast
and works even when a server is down. Each tool is named `<connector>.<tool>`,
for example `linear.create_issue`.

Search is **hybrid**. Fuzzy and full-text matching always run. If you configure
an embeddings endpoint, the daemon also embeds each tool's name, title,
description, and parameters, and adds the nearest neighbors of the query. Both
result lists are merged by reciprocal-rank fusion into one ranking that stays
the same from page to page. With no embeddings, or when the endpoint is down,
search uses only fuzzy and full-text matching. See
[ADR 0005](/adr/0005-store-owns-hybrid-search) and
[How Search Works](/guide/search).

`call_tool` checks the arguments against the tool's cached input schema, then
forwards the call to the upstream and returns its result unchanged. Failures on
Wayfindr's side, such as an unknown tool, bad arguments, a missing
authorization, or a timeout, come back as tool results with `isError: true` and
a message that says what to do. The agent can read them and recover. See
[ADR 0001](/adr/0001-two-meta-tools).

Every `call_tool` also goes into the **call log** in the store, never with its
arguments or result. See [Monitoring](/guide/monitoring#what-did-agents-call).

## Limiting what agents reach

By default an agent can find and call every tool of every connector. Four
layers narrow that, checked on every request in `search_tools` and `call_tool`
alike: the client token, the connector's tools list, a profile, and a project
scope. A tool is in reach only when every layer allows it, so a hidden tool
never shows up in search and is refused by name. See
[Control what agents can use](/guide/access/) for how they combine and
[ADR 0009](/adr/0009-tool-policy-in-the-gateway),
[0010](/adr/0010-project-scopes), [0011](/adr/0011-named-profiles), and
[0012](/adr/0012-scoped-client-tokens) for why.

One more check applies to `call_tool` only, and only to remote clients: a remote
client token or an OAuth grant can be created with **approval entries**. A call
that an entry matches is refused until you approve it, in the console or with
`wayfindr approvals`, which a remote client can't reach. The gate runs after the
layers above and before the upstream is touched. Wayfindr holds the call's
arguments in memory, shows them to you as it received them, and an approval
covers one call with exactly those arguments. The default token and tokens made
without `--remote` have no gate, because a local client could approve its own
calls with the default token. The gate keys on the token, not the machine: a
remote token used locally is held too. See
[Approval gates](/guide/access/scoped-tokens#approval-gates) and
[ADR 0024](/adr/0024-approval-gates-on-remote-clients).

## Credentials

The daemon presents each connector's **credential**: OAuth tokens, static
headers such as an API key, or nothing. Clients never see credentials.

For OAuth, the daemon finds the server's endpoints by discovery, registers a
client if it needs one, and runs the sign-in when you run `wayfindr auth login` or
open the connector's auth link. See [Authentication](/guide/authentication) and
[ADR 0007](/adr/0007-oauth-discovery-and-registration).

Tokens are refreshed only when a call needs them, one refresh at a time per
credential, so a burst of calls never spends a rotating refresh token twice. See
[ADR 0004](/adr/0004-lazy-single-flight-refresh).

Config values can reference secrets (`${secret:name}`) and environment variables
(`${env:NAME}`). Secrets, tokens, and registered clients are stored encrypted
under a master key. See [Secrets](/guide/secrets) and
[Security](/guide/security).

## Connector states

Each connector is in exactly one state: `starting`, `ready`, `needs_auth`,
`auth_error`, or `unreachable`. `wayfindr status` shows it, and `search_tools`
lists every connector that isn't `ready` in its `unavailable` list, with the
same advice. [Connector states](/guide/troubleshooting#connector-states)
explains each one.

When you fix something, such as authorizing a connector or setting a missing
secret, the daemon reconnects the affected connectors at once and waits for
their first sync, so the command that made the fix can report them.

## Configuration and reload

All configuration is one TOML file, `~/.config/wayfindr/wayfindr.toml`. The
daemon watches it and applies each save to what changed, so connectors you didn't
touch keep their connections. If a save is invalid, it keeps running the last
good config. [Reloading](/reference/configuration#reloading) lists what each
change does. See [ADR 0006](/adr/0006-toml-config).

## Storage

The store holds the catalog, the embeddings, the encrypted secrets and
credentials, the hashes of scoped client tokens, and the call log. SQLite is the
default and needs no setup. Postgres with pgvector is the alternative. Each
store runs hybrid search itself. See [Storage](/guide/storage).

## Source layout

For contributors: Wayfindr is one Go module, with `cmd/wayfindr` calling into
packages under
[`internal/`](https://github.com/atomicpages/wayfindr/tree/main/internal).
Dependencies point one way, from the CLI through the daemon down to the store,
secrets, and config, and events travel back up through callbacks that the daemon
wires together. The full technical design, with the package map, is
[docs/internal/design.md](https://github.com/atomicpages/wayfindr/blob/main/docs/internal/design.md)
in the repository.
