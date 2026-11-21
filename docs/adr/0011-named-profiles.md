# 0011 — Pick a scope by name with profiles in the config

Status: Accepted (2026-11-11). Amends the no-shim consequence of
[0010](./0010-project-scopes), and the refusal order of
[0009](./0009-tool-policy-in-the-gateway): rules that hide a tool by name are
checked before the catalog lookup. Amended:

- 2026-11-11 (wayfindr-5rbg): the client token is now the outer limit a profile
  narrows within, and a scoped client token allows only its connectors
  ([0012](./0012-scoped-client-tokens)).

## Context

A project scope ([0010](./0010-project-scopes)) reaches the gateway only through
the stdio shim: it reads `.wayfindr.toml` and sends it in the `Wayfindr-Scope`
header. Clients that connect to `/mcp` over HTTP directly, such as Cursor, Zed,
and some VS Code setups, never run the shim, so they get no scope at all.

Those clients do have per-project config of their own: `.cursor/mcp.json`,
`.mcp.json`, `.codex/config.toml`. A project can say something there, but only
in what the client sends: a command line for a stdio server, a URL and headers
for an HTTP one. The daemon still doesn't know which directory a client works
in.

The repository is untrusted, as in [0010](./0010-project-scopes). Whatever a
project writes must only narrow, and must not be able to define what it narrows
to.

## Decision

- A profile is a named scope in the user's config: `[profiles.<name>]`, with the
  keys and rules of `.wayfindr.toml` (a `connectors` allowlist and per-connector
  tools lists). Names follow the connector-name rule.
- A client picks a profile by name only. `wayfindr stdio --profile <name>` sends
  it in a `Wayfindr-Profile` header; a client on HTTP adds `?profile=<name>` to
  the `/mcp` URL, which the gateway reads as the same header. The project names
  a profile in the client's own per-project config; it can never define one.
- The daemon resolves the name on every request against the running config. An
  edit to a profile applies at the next reload, with no reconnect.
- A profile and a scope file are ANDed. A tool is exposed when the connector's
  tools list, the profile, and the file all expose it. The gateway checks them
  in that order, and the first that hides a tool decides the Case:
  `hidden_by_policy`, `hidden_by_profile`, or `hidden_by_scope`. Rules that hide
  a tool by name are checked first, before the catalog is read, so a call learns
  nothing about a tool it may not use; rules by class follow.
- A profile that names a connector the config lacks is a config validation
  error, not a warning as in a scope file. The profile is the user's own, so it
  is a typo or left over, and the reload keeps the last good config.
- It fails closed. A name the running config doesn't define refuses every
  request with `unknown_profile`, rather than serving the client unscoped. A
  name sent more than once (header and query, or repeated) or not a valid name
  is refused as `invalid_scope`, and an invalid name is not echoed. So is a
  `/mcp` query that doesn't parse, or that spells the key in another case such
  as `Profile`, since either would drop the profile. `wayfindr stdio --profile`
  checks the name's syntax before it starts.
- `wayfindr scope --profile <name>` shows the profile from the running daemon,
  ANDed with the scope file that applies in the directory.

## Consequences

- Clients without the shim can be narrowed per project, with the same rules as a
  scope file.
- What a profile allows lives in the user's config, not the repository, so a
  team can't share one by committing it. A committed client config can name a
  profile; a teammate without that profile gets every request refused until they
  define it.
- Trusting the client to send its profile is safe for the same reason as in
  [0010](./0010-project-scopes): a client that names a profile only narrows
  itself, and one that leaves it off gets what its client token already allows.
- A profile applies without a reconnect, while a scope file applies when the
  shim starts. Editing a profile changes every client that uses it at once.
- The call log records a refusal by a profile under its own Case, apart from the
  config's tools lists and the scope file.
- There is no default profile, and profiles don't inherit from each other.
