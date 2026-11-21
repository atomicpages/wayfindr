# 0008 — Relay elicitation to clients

Status: Accepted (2026-11-09). Amends the elicitation part of
[0003](./0003-single-daemon) and the out-of-scope consequence of
[0001](./0001-two-meta-tools).

## Context

More and more MCP servers ask the user something in the middle of a tool call:
which workspace, whether to delete 40 files, or to open a URL to connect an
account. This is elicitation. [0003](./0003-single-daemon) declined it, so every
such call failed with "requires interactive input".

The daemon's `/mcp` endpoint is stateless, so it can't send a request to a
client while a call waits. Protocol 2026-07-28 removed the need: a `tools/call`
can return `input_required` with the questions and an opaque `requestState`, and
the client calls again with the answers. Upstreams at 2025-x still ask with an
`elicitation/create` request during the call, and the daemon holds their
sessions, so it can receive the request.

## Decision

- Toward upstreams, the daemon advertises elicitation (form and URL mode). It
  still advertises neither sampling nor roots, and declines sampling.
- An upstream at 2026-07-28 returns `input_required`, which the daemon passes
  on.
- An upstream at 2025-x sends `elicitation/create` during the call. The daemon
  parks it as an interaction and returns the question to the client as
  `input_required`. The upstream call keeps running. Its connector timeout is
  paused while the user answers, for at most 10 minutes. Then the upstream is
  told the user cancelled.
- The request carries nothing that names the call it belongs to. So the daemon
  accepts the question only while that connector has exactly one call in flight,
  a parked one included. Otherwise it declines, and the call fails saying why.
- `call_tool` returns `input_required` to clients at 2026-07-28 that declare
  elicitation. Each question's id and message name the connector. The
  `requestState` is sealed with the master key and bound to the user, tool,
  arguments, and an expiry. An answer that doesn't match is refused.
- Older HTTP clients, and clients without elicitation, get an error that points
  at `wayfindr stdio`. The shim passes `input_required` through to a client at
  2026-07-28. It asks an older client directly with `elicitation/create`, which
  stdio allows.

## Consequences

- Elicitation works for every client that connects through `wayfindr stdio`, and
  for HTTP clients at 2026-07-28.
- HTTP clients at 2025-x still can't be asked. That would need a stateful
  `/mcp`.
- A question nobody answers keeps its upstream call, and the session lease, for
  up to 10 minutes. Meanwhile, more 2025-x questions on that connector are
  declined.
- No page on the daemon asks the user. The client is the only UI.
