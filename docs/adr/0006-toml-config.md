# 0006 — Declarative TOML config; secrets live encrypted in the store

Status: Accepted (2026-11-03). Amended:

- 2026-11-04 (wayfindr-4fr): the master key has no local or server mode. The
  daemon tries `WAYFINDR_SECRET_KEY`, then the OS keychain, then
  `<data>/master.key`.
- 2026-11-04 (wayfindr-i2a): a change to `[server]` or `[storage]` takes effect
  only on a restart.
- 2026-11-06 (wayfindr-j99t): a changed `url` sets `needs_auth` only on OAuth
  connectors; other connectors reconnect and sync.
- 2026-11-08 (wayfindr-gt5k), 2026-11-09 (wayfindr-c06s), 2026-11-12
  (wayfindr-3e6f): the CLI writes the config too, one connector at a time, and
  `connector import` stores literal values as secrets.
- 2026-11-12 (wayfindr-3e6f): a hand edit or reload still keeps a removed
  connector's credential, but `wayfindr connector remove` is an explicit delete.
- 2026-11-12 (wayfindr-h0p8): the daemon writes the config too, to add a
  connector for the web console ([0017](./0017-console-adds-connectors)).

## Context

Users want connectors in their dotfiles. Tokens and API keys must not end up in
those dotfiles.

## Decision

- `wayfindr.toml` is the source of truth for connectors and settings, and is
  hot-reloaded. A change to `[server]` or `[storage]` is the exception: it takes
  effect when the daemon restarts (amended 2026-11-04, wayfindr-i2a). TOML was
  chosen over YAML: indentation mistakes can't silently break it, Codex users
  already know it, and keyed tables (`[connectors.<name>]`) make connector names
  unique by construction.
- The user edits the file, and the CLI writes it too (amended 2026-11-08,
  wayfindr-gt5k; 2026-11-09, wayfindr-c06s; 2026-11-12, wayfindr-3e6f).
  `wayfindr connector add` appends one connector's table, `connector remove`
  deletes one, and `connector edit` changes only the keys it names. Everything
  else in the file is kept byte for byte. The CLI refuses an edit it can't make
  safely: a file that doesn't parse, connectors written as an inline table, or a
  result that would read differently in anything but that connector.
  `wayfindr connector import` stores literal `env` and `headers` values, and
  `${VAR}` values its shell sets, as secrets, and writes `${secret:…}`
  references to them. The daemon also appends a connector's table, the same way,
  when the web console adds one (amended 2026-11-12, wayfindr-h0p8,
  [0017](./0017-console-adds-connectors)).
- A connector with `url` is remote. A connector with `command` is stdio. API-key
  auth is just a `headers` table.
- Values can reference `${env:NAME}` or `${secret:name}`. Secrets (set with
  `wayfindr secret set`), OAuth tokens, and client secrets are stored encrypted
  with AES-GCM. The key comes from the OS keychain in local mode, or from
  `WAYFINDR_SECRET_KEY` in server mode (amended 2026-11-04, wayfindr-4fr: there
  is no mode. The daemon takes the first key it finds in `WAYFINDR_SECRET_KEY`,
  the OS keychain, and `<data>/master.key`, a file only its owner may read
  (0600). `wayfindr init` creates a key in the keychain, or in the file when the
  keychain is unavailable).
- On reload:
  - A **removed** connector's catalog entries are deleted immediately, but its
    credentials are kept until `wayfindr auth revoke <name>`. This holds for
    hand edits and reloads, so commenting a connector out doesn't force
    re-authentication. `wayfindr connector remove` is an explicit delete
    instead: it also deletes the connector's credential and every secret that
    nothing else in the config references (amended 2026-11-12, wayfindr-3e6f).
    `wayfindr connector edit` deletes the credential too when it removes
    `[auth]` or its `client_id`.
  - A changed **`url`** sets the connector to `needs_auth`, because tokens are
    bound to the old resource (RFC 8707). Only an OAuth connector does; others
    reconnect and sync (amended 2026-11-06, wayfindr-j99t).
  - Other changes cause a reconnect and a sync.

## Consequences

- Temporarily commenting out a connector doesn't force re-authentication.
- Removed connectors' credentials stay around until revoked explicitly.
  `wayfindr status` should list them.
- Removing a connector with the CLI leaves no credential or unshared secret
  behind; remove and add it again, and it needs a new sign-in (amended
  2026-11-12, wayfindr-3e6f).
