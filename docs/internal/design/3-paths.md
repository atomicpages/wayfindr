# §3 Paths and identity

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §3".

| Item         | Path (env override)                                                                                                 |
| ------------ | ------------------------------------------------------------------------------------------------------------------- |
| Config       | `$XDG_CONFIG_HOME/wayfindr/wayfindr.toml`, else `~/.config/…` on every OS (`--config`, then `WAYFINDR_CONFIG`)      |
| Data dir     | `$XDG_DATA_HOME/wayfindr/`, else `~/.local/share/wayfindr/` (`--data-dir`, then `WAYFINDR_DATA_DIR`)                |
| SQLite DB    | `<data>/wayfindr.db` (default `storage.dsn`), created mode 0600                                                     |
| Client token | `<data>/client-token`, mode 0600 (no path override; see below)                                                      |
| Key file     | `<data>/master.key`, mode 0600; used only when no keychain is available                                             |
| Daemon lock  | `<data>/daemon.lock`, mode 0600; held by the running daemon ([§7.8](7.8-daemon.md))                                 |
| Daemon log   | `<data>/wayfindr.log`, mode 0600; a background daemon's output ([§7.9](7.9-cli.md))                                 |
| Telemetry ID | `<data>/telemetry-id`, mode 0600; the usage report's random install ID, written only once telemetry runs (ADR 0020) |
| Scope file   | `.wayfindr.toml` in the shim's working directory or a parent (below)                                                |

- **Path flags.** `paths.Resolve` reads only its `Env`. The CLI's `--config`
  and `--data-dir` reach it by overlaying that `Env` (`WAYFINDR_CONFIG` and
  `WAYFINDR_DATA_DIR` read as the flags, made absolute), not through a second
  resolution path ([§7.9](7.9-cli.md)). A daemon `daemonctl` starts is given
  its starter's resolved config file and data directory in those variables, and
  `service install` writes the overlaid values into the unit, so the processes
  Wayfindr starts resolve the same files.
- **User.** `wayfindr init` creates one `users` row with a UUIDv7 id. Local mode
  always uses that row. Team mode (deferred) maps OIDC `(issuer, subject)` to
  user ids in a separate `user_identities` table.
- **Client token.** 32 random bytes, base64url-encoded, prefixed `wf_`, redrawn
  if it would begin `wf_s_`, the scoped-token prefix the daemon refuses as the
  default (`clienttoken.GenerateDefault`). The daemon reads it at startup and
  compares in constant time. `WAYFINDR_CLIENT_TOKEN`, when set, holds the token
  **value** (like `WAYFINDR_SECRET_KEY`) and takes precedence over
  `<data>/client-token`, for CI and containers. `init` still writes the file
  only when it is absent. This is the **default client token**. **Scoped client
  tokens** (ADR 0012) have no file: `wayfindr token create` prints the value
  once (`wf_s_` + base64url of 32 random bytes) and the store keeps only its
  SHA-256 ([§5](5-data-model.md)). A client presents either kind the same way,
  so the env override also carries a scoped value to the shim. The daemon
  refuses to start when its default token, from either source, is a scoped value
  (`clienttoken.IsScoped`), naming the source but never the value: that token
  would otherwise get every connector and the admin API. For the same reason
  `daemonctl` starts a daemon with `clienttoken.WithoutScoped` of its
  environment, so a client running `wayfindr stdio` with a scoped
  `WAYFINDR_CLIENT_TOKEN` can still start one, which then reads the file.
- **Project scope file.** `config.LoadScope(dir, home)` walks from `dir` (the
  shim's working directory) up through its parents and takes the nearest
  `.wayfindr.toml`, stopping before the home directory, so a file in `~` never
  applies to every project; with no home it walks to the root. A file that can't
  be read or doesn't validate is an error, never skipped, since skipping it
  would widen what the project gets. Only the shim reads it (and
  `wayfindr scope`, to show it); the daemon gets it in the `Wayfindr-Scope`
  header ([§7.6](7.6-gateway.md)).
