# 0001 — Expose exactly two meta-tools: `search_tools` and `call_tool`

Status: Accepted (2026-11-03). Amended:

- 2026-11-04 (wayfindr-987): each `unavailable` entry also carries advice:
  `summary`, `action`, and `link`.
- 2026-11-08 (wayfindr-5b5v): `search_tools` gained `detail` (`"brief"` returns
  compact entries without schemas, up to 25 per page) and `names` (full
  definitions of exact qualified names, at most 10), which reports names it
  can't find under `missing`. Still two tools.
- 2026-11-08 (wayfindr-1dq8): the response always has a `warnings` list
  ([0015](./0015-credential-health-warnings)).
- 2026-11-09 (wayfindr-7a8p): elicitation is relayed to clients
  ([0008](./0008-relay-elicitation)).
- 2026-11-09 (wayfindr-t1p1): the per-tool allow/deny granularity this record
  gave up moves into the daemon as a per-connector `tools` list
  ([0009](./0009-tool-policy-in-the-gateway)). Annotations reach the agent, but
  no client reads them to prompt for `call_tool`.

## Context

Exposing every upstream tool in `tools/list` floods the agent's context, which
is the problem Wayfindr exists to solve. MCP only lets a client call tools that
appear in `tools/list`. Adding search hits to `tools/list` and sending
`list_changed` is handled unevenly by harnesses, breaks prompt caching, and
under 2026-07-28 needs a held-open subscription stream.

## Decision

Wayfindr's `tools/list` contains exactly two tools:

- `search_tools(query, page?, size?, connectors?)` returns full catalog entries
  (`name`, `title`, `description`, `inputSchema`, `outputSchema`,
  `annotations`), ranked by hybrid search. `size` defaults to 5, maximum 10. The
  response also has `page`, `size`, `has_more`, and an `unavailable` list of
  connectors that are not `ready`. Each `unavailable` entry says what to do
  about it, with `summary`, `action`, and `link` (amended 2026-11-04,
  wayfindr-987). `detail: "brief"` returns compact entries without schemas, 15
  per page by default and at most 25. `names` returns the full entries of up to
  10 exact qualified names, and lists those it can't find under `missing`
  (amended 2026-11-08, wayfindr-5b5v). The response always has a `warnings`
  list: credential failures due within a day on ready connectors, so the agent
  can tell the user before a call fails (amended 2026-11-08, wayfindr-1dq8; see
  [0015](./0015-credential-health-warnings)).
- `call_tool(name, arguments)` is the primary way to invoke a tool. `name` is
  the qualified name. Arguments are validated against the cached `inputSchema`
  before being forwarded. The upstream `CallToolResult` is returned unchanged.
  Gateway-side failures (unknown tool, invalid arguments, `needs_auth`,
  unreachable, timeout) are returned as `isError: true` results, not as JSON-RPC
  errors, so the agent can read them and correct course.

Upstream `annotations` are passed through so that client permission prompts
(e.g. on `destructiveHint`) keep working.

## Consequences

- Context cost is bounded: at most 10 full definitions per search page.
- Client-side permission rules that match on tool names see `call_tool`, not
  `linear.create_issue`. Users who rely on per-tool allow/deny lists in their
  harness lose that granularity. A "pinned tools" list exposed directly is the
  escape hatch if this matters. It is deferred. (Amended 2026-11-09,
  wayfindr-t1p1: a per-connector `tools` list in the daemon decides which tools
  agents see; see [0009](./0009-tool-policy-in-the-gateway).)
- Only tools are proxied. Resources, prompts, sampling, elicitation, and roots
  are out of scope for v1. See [0003](./0003-single-daemon). (Amended
  2026-11-09, wayfindr-7a8p: elicitation is relayed to clients; see
  [0008](./0008-relay-elicitation).)
