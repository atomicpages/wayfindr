# 0009 — Choose which upstream tools agents see with a per-connector tools list

Status: Accepted (2026-11-09). Amends the per-tool permission consequence of
[0001](./0001-two-meta-tools). Amended:

- 2026-11-10 (wayfindr-4n35): a project scope narrows these rules per project
  ([0010](./0010-project-scopes)).
- 2026-11-11 (wayfindr-bsgy): rules that hide a tool by name are checked before
  the catalog lookup ([0011](./0011-named-profiles)).
- 2026-11-11 (wayfindr-5rbg): scoped client tokens limit connectors only;
  per-client tool rules were deferred ([0012](./0012-scoped-client-tokens)).

## Context

Every upstream tool is called through `call_tool`, so a client's permission
rules see one tool. In Claude Code the rule matches `mcp__wayfindr__call_tool`:
the user either allows it, which allows `github.delete_repository` too, or
approves every call by hand, reads included. [0001](./0001-two-meta-tools)
accepted this loss and deferred it.

Upstream annotations reach the agent in `search_tools` results, but no client
reads them to decide a prompt for `call_tool`. The daemon is the one place that
sees every tool and its annotations.

The common wish is "read everything, plus a few chosen writes": Gmail with
drafts but never sending. Separate allow, deny, and destructive switches can't
say that without listing every read tool by name.

## Decision

- Each connector may have a `tools` list. An entry is a class (`@read`,
  `@write`), a glob on the upstream tool name, or a `!glob` that hides the tools
  it matches. A `!` match always wins. A list with only `!` entries exposes the
  rest; otherwise a tool must match a class or a glob. No list exposes every
  tool.
- The class comes from the upstream's annotations with MCP's defaults: `read`
  when `readOnlyHint` is true, `write` when `destructiveHint` is explicitly
  false, and `destructive` otherwise, including tools without annotations.
  `@write` includes `@read`.
- The Syncer filters: hidden tools never reach the catalog, so search pages stay
  full. The sync record keeps the hidden tools' names and classes, for status
  and for errors.
- The gateway also checks the live list, because after a reload the catalog
  holds newly hidden tools until the connector's re-sync succeeds, which may be
  never while it is down. `search_tools` drops them (that page comes back
  short), and `call_tool` refuses them after the catalog lookup and before the
  upstream is called or an answer is passed on, with `unknown_tool` and Case
  `hidden_by_policy`. (Amended 2026-11-11, wayfindr-bsgy: a rule that hides a
  tool by name is checked before the catalog lookup, so a call learns nothing
  about a tool it may not use; rules by class still follow the lookup. See
  [0011](./0011-named-profiles).)
- A change to `tools` makes the connector changed in the config diff: it
  reconnects and re-syncs. Only a re-sync is needed, but the extra reconnect is
  correct and avoids a new change bucket.
- `wayfindr connector tools <name>` lists the upstream's tools live, with class
  and exposure, so users can write the list.

## Consequences

- Users can give an agent a connector's reads and a few named writes, and hide
  destructive tools, without per-call prompts.
- Classes are only as good as the upstream's annotations. A server that sends
  none has no `@read` or `@write` tools; one that mislabels a tool exposes it
  under a class. The docs say to name writes rather than trust `@write`.
- Rules don't vary per client or per argument value. Per-client rules belong to
  scoped client tokens. (Amended 2026-11-11, wayfindr-5rbg: scoped client tokens
  limit a client to named connectors only; per-token tool rules are a possible
  follow-up. See [0012](./0012-scoped-client-tokens).)
- Asking the user before a chosen tool runs (a `confirm` entry using
  elicitation, now relayed per [0008](./0008-relay-elicitation)) is a possible
  follow-up and not part of this decision.
