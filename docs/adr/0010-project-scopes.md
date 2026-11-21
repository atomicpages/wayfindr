# 0010 — Narrow a project's connectors and tools with a project scope file

Status: Accepted (2026-11-10). Amends the per-client consequence of
[0009](./0009-tool-policy-in-the-gateway). Amended:

- 2026-11-11 (wayfindr-bsgy): clients without the shim pick a named profile from
  the config instead ([0011](./0011-named-profiles)).
- 2026-11-11 (wayfindr-5rbg): the client token is now the outer limit a scope
  narrows within, and a scoped client token allows only its connectors
  ([0012](./0012-scoped-client-tokens)).

## Context

The tool policy is global: one daemon serves every client
([0003](./0003-single-daemon)) with the `tools` lists in the user's config
([0009](./0009-tool-policy-in-the-gateway)). A developer often wants less in one
project than everywhere else: in this repository, the agent gets GitHub reads
and pull requests, and Linear, and nothing else.

A file in the project is the natural place for that, and it can be committed so
a team shares it. But a file in a repository is untrusted: anyone who clones the
repository gets it. If it could define a connector, a stdio `command` would run
code on the user's machine. If it could widen a tools list, it would expose a
tool the user hid on purpose.

The daemon doesn't know which directory a client works in. The stdio shim does:
clients start it in the project. The 2026-07-28 protocol is stateless, so the
scope can't be set once per session.

## Decision

- A project scope is a `.wayfindr.toml` with an optional `connectors` allowlist
  and a `[tools]` table of per-connector tools lists, in the same syntax as the
  config's. It can only narrow. A tool is exposed when the config's policy
  exposes it and the scope does. The file has no other keys, so it can't name a
  command, URL, header, auth setting, or secret.
- `wayfindr stdio` looks for the nearest `.wayfindr.toml` from its working
  directory up, stopping before the home directory, so a file there never
  applies to every project. The nearest file wins; files are not merged.
- The shim sends the validated scope as compact JSON in a `Wayfindr-Scope`
  header on every request. The gateway applies it per request. In
  `search_tools`, connectors outside the allowlist are as if not configured, the
  allowlist bounds the store's search so pages stay full, and the scope's tools
  lists drop tools from the page. `call_tool` refuses a hidden tool with
  `unknown_tool` and Case `hidden_by_scope`.
- Trusting the client to send its scope is safe: a client that narrows itself
  gains nothing, and one that leaves the header off gets only what its client
  token already allows.
- It fails closed. A scope file that doesn't validate stops the shim, and a
  header that doesn't parse is refused with `invalid_scope`. Ignoring either
  would give the project more than it asked for.
- `wayfindr scope` shows the file that applies in a directory, which configured
  connectors it allows, and, for one connector, which tools both lists expose.

## Consequences

- A team can commit a project's limits. A hostile scope file can at worst hide
  the user's own tools.
- Clients that connect to `/mcp` directly, with no shim, get no project scope.
  Named profiles in the user's config, chosen per project in the client's own
  config, are a possible follow-up for them.
- The scope's tools lists are applied after the search, so a page can come back
  short by the tools they hide, as after a reload under
  [0009](./0009-tool-policy-in-the-gateway).
- The shim reads the file when it starts. An edit applies when the client
  reconnects to it.
- The call log records a refusal by the scope under its own Case, so it is not
  confused with the config's policy.
