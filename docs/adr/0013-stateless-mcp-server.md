# 0013 — Serve `/mcp` statelessly at every protocol version

Status: Accepted (2026-11-04). Written down on 2026-11-12 (wayfindr-q2rb); the
decision shipped in `ff03dd3` (wayfindr-987). Amended:

- 2026-11-09 (wayfindr-7a8p): `call_tool` can return `input_required`, the one
  way the daemon asks a client something ([0008](./0008-relay-elicitation)).

## Context

Clients speak MCP 2025-06-18, 2025-11-25, or 2026-07-28
([0003](./0003-single-daemon)). The 2025 versions have an initialize handshake
and sessions: an `Mcp-Session-Id`, and a GET stream on which the server sends
requests and notifications. 2026-07-28 removes both. Every request stands alone.

A stateful endpoint would have to hold a session per client, keep it across
reloads, and expire the ones clients abandon. It would also have to serve
2026-07-28 clients without one. The daemon already holds the stateful upstream
sessions itself, so clients don't need sessions for that.

Wayfindr exposes two tools that never change ([0001](./0001-two-meta-tools)), so
it has nothing to push to a client.

## Decision

- The gateway serves `/mcp` with one MCP SDK Streamable HTTP handler in
  stateless mode, for every client protocol version. It neither reads nor sets
  `Mcp-Session-Id`, and handles each POST on its own. GET and DELETE answer 405.
- The server declares only `tools`, without `listChanged`, and no `logging`. A
  stateless handler can deliver neither, and the SDK would otherwise advertise
  both. `prompts/*`, `resources/*`, and `completion/*` answer -32601 "method not
  found", as from a server that never implemented them.
- The server sends clients no requests and no notifications: no sampling,
  elicitation, or roots requests, no `list_changed`, and no log messages.
  (Amended 2026-11-09, wayfindr-7a8p: an upstream's question reaches the client
  as an `input_required` result of `call_tool`, at 2026-07-28; see
  [0008](./0008-relay-elicitation).)
- Everything a request needs comes with it: the client token, and the
  `Wayfindr-Scope` and `Wayfindr-Profile` headers. Nothing is set once per
  session.

## Consequences

- The daemon keeps no state per client. A reload or a restart loses no client
  session, because there is none.
- A client that waits for `list_changed` never gets one. It needs none: the two
  tools never change, and catalog changes reach agents through `search_tools`.
- The daemon's log messages go to its own log (`wayfindr logs`), not to clients.
- A 2025-x client on HTTP can't be asked a question during a call. Elicitation
  needs 2026-07-28 or the stdio shim ([0008](./0008-relay-elicitation)).
- Narrowing per request works because nothing is per session: project scopes
  ([0010](./0010-project-scopes)), profiles ([0011](./0011-named-profiles)), and
  scoped client tokens ([0012](./0012-scoped-client-tokens)) apply to each
  request on its own.
