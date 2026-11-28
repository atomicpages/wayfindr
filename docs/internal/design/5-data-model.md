# §5 Data model

Part of the [Wayfindr technical design](../design.md). Go code cites this file
as "design §5".

Every table has a `user_id`. The schema below is the SQLite version. Postgres
uses `uuid`, `bytea`, `timestamptz`, and `vector`. Timestamps are Unix
milliseconds in SQLite. Columns marked _sealed_ hold `secrets.Sealer`
ciphertext.

```sql
CREATE TABLE schema_version (version INTEGER NOT NULL);  -- created by the migration runner (§6)

CREATE TABLE users (
  id         TEXT PRIMARY KEY,          -- UUIDv7
  created_at INTEGER NOT NULL
);

CREATE TABLE tools (
  user_id         TEXT NOT NULL REFERENCES users(id),
  connector       TEXT NOT NULL,
  name            TEXT NOT NULL,        -- upstream (unqualified) tool name
  definition      BLOB NOT NULL,        -- mcp.Tool JSON, SDK-normalised (see below)
  content_hash    BLOB NOT NULL,        -- sha256(embed text)
  embedding       BLOB,                 -- little-endian float32[], NULL until embedded
  embedding_model TEXT,                 -- model identity that produced `embedding`
  updated_at      INTEGER NOT NULL,
  PRIMARY KEY (user_id, connector, name)
);

CREATE TABLE connector_sync (
  user_id      TEXT NOT NULL,
  connector    TEXT NOT NULL,
  last_sync_at INTEGER,
  last_error   TEXT,
  hidden_tools TEXT,     -- JSON [{name, class}] the tool policy hid; kept on a failed sync
  PRIMARY KEY (user_id, connector)
);

CREATE TABLE credentials (
  user_id          TEXT NOT NULL,
  connector        TEXT NOT NULL,
  resource         TEXT NOT NULL,       -- "sha256:<hex>" of the MCP URL the tokens are bound to (RFC 8707); never the URL, which may hold a secret (wayfindr-wt2v)
  issuer           TEXT,                -- authorization server issuer
  redirect_uri     TEXT,                -- URI the DCR client was registered with
  dcr_client_id    TEXT,                -- set only when obtained via DCR
  dcr_client       BLOB,                -- sealed: DCR client_secret + registration response
  token            BLOB,                -- sealed: {access, refresh, type, expiry}
  granted_scopes   TEXT,
  requested_scopes TEXT,                -- includes step-up additions
  updated_at       INTEGER NOT NULL,
  client_id        TEXT,                -- the client the grant was obtained with, manual or DCR; NULL: unknown
  PRIMARY KEY (user_id, connector)
);

CREATE TABLE secrets (
  user_id    TEXT NOT NULL,
  name       TEXT NOT NULL,
  value      BLOB NOT NULL,             -- sealed
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (user_id, name)
);

CREATE TABLE oauth_metadata (
  url        TEXT PRIMARY KEY,          -- document URL actually fetched
  kind       TEXT NOT NULL,             -- 'prm' | 'as'
  document   BLOB NOT NULL,
  fetched_at INTEGER NOT NULL
);

CREATE TABLE client_tokens (            -- scoped client tokens (ADR 0012)
  user_id      TEXT NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,             -- connector-name rule; never 'default'
  digest       BLOB NOT NULL UNIQUE,      -- SHA-256 of the token, 32 bytes
  connectors   TEXT NOT NULL,             -- JSON array of connector names, at least one
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,                   -- NULL until first use
  remote       INTEGER NOT NULL DEFAULT 0, -- 1: may reach /mcp through a tunnel (ADR 0021); fixed at creation
  PRIMARY KEY (user_id, name)
);

CREATE TABLE oauth_grants (             -- OAuth grants (ADR 0022); names shared with client_tokens
  user_id      TEXT NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,             -- connector-name rule; never 'default'; no client_tokens row has it
  client_id    TEXT NOT NULL,             -- the allowed client's CIMD URL
  resource     TEXT NOT NULL,             -- <public_url>/mcp when approved (RFC 8707)
  connectors   TEXT NOT NULL,             -- JSON array of connector names, at least one
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,                   -- NULL until its access token is first used
  PRIMARY KEY (user_id, name)
);

CREATE TABLE oauth_tokens (             -- a grant's access and refresh tokens
  digest       BLOB PRIMARY KEY,          -- SHA-256 of the token, 32 bytes
  user_id      TEXT NOT NULL,
  grant_name   TEXT NOT NULL,             -- `grant` is an SQL keyword
  kind         TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,                   -- access: set; refresh: NULL (no expiry)
  used_at      INTEGER,                   -- refresh: when it was spent; NULL until then
  FOREIGN KEY (user_id, grant_name) REFERENCES oauth_grants (user_id, name) ON DELETE CASCADE
);

CREATE INDEX oauth_tokens_grant ON oauth_tokens (user_id, grant_name);

CREATE TABLE calls (                    -- the call log: one row per call_tool call
  id          TEXT PRIMARY KEY,           -- UUIDv7
  user_id     TEXT NOT NULL,
  client      TEXT,                       -- the calling client's clientInfo name; NULL when unknown
  connector   TEXT NOT NULL DEFAULT '',   -- as the agent wrote it
  tool        TEXT NOT NULL DEFAULT '',   -- as the agent wrote it
  started_at  INTEGER NOT NULL,
  duration_ms INTEGER,                    -- NULL when the upstream was not called
  result      TEXT NOT NULL,              -- 'ok' | 'tool_error' | 'input_required' | 'rejected' | 'failed'
  error_kind  TEXT,
  error_case  TEXT,
  token_name  TEXT,                       -- 'default' or a scoped token's name (added by 0004)
  error_text  TEXT,                       -- the error the agent got back; NULL when ok (added by 0006)
  result_bytes INTEGER                    -- size of the result the agent got back; NULL when none (added by 0009)
);

CREATE INDEX calls_user_started ON calls (user_id, started_at DESC, id DESC);
```

- **Tool definitions** are stored SDK-normalised: the `*mcp.Tool` the SDK
  decoded, marshalled again, not the upstream's bytes. Fields the SDK doesn't
  model are dropped. `content_hash` is `sha256(embed text)`
  ([§7.4](7.4-catalog.md)), so it doesn't depend on that encoding.
- **A sync is one write.** `tools` and `connector_sync` are written in one
  transaction (`ApplySync`, [§6](6-store.md)), so a connector's catalog and its
  last-sync record never disagree, and removing a connector deletes both in one
  transaction (`DeleteCatalog`).
- **Config-provided client secrets** are never copied into `credentials`. They
  are resolved from `${secret:}` each time they're needed.
- **Credentials are kept when a connector is removed** (ADR 0006), by a reload
  and by the startup that drops a removed connector's catalog. `wayfindr status`
  lists credential rows that have no matching connector as orphans, until
  `wayfindr auth logout <connector>` deletes them.
- **URL changes.** On load and on reload, if the canonical `url` differs from
  `credentials.resource`, the connector goes to `needs_auth`. The stale token is
  never sent to the new URL.
- **Redirect URI changes.** If the effective `redirect_uri` differs from
  `credentials.redirect_uri`, the DCR client is re-registered.
- **Scoped client tokens** (ADR 0012) are stored as a SHA-256 digest only, never
  the value. The primary key `(user_id, name)` makes names unique per user; the
  digest is unique across users, so a future team mode still maps one value to
  one user. Postgres stores `connectors` as `text[]` and checks
  `octet_length(digest) = 32` and `cardinality(connectors) > 0`. The migrations
  are SQLite `0004_client_tokens.sql` and Postgres `0008_client_tokens.sql`.
  `remote` (ADR 0021; `boolean` in Postgres) is set at creation and never
  updated; SQLite `0007_client_token_remote.sql` and Postgres
  `0011_client_token_remote.sql` add it, false for earlier tokens.
  `CreateClientToken` refuses a duplicate name or digest with `ErrExists`;
  `TouchClientTokens` is keyed by digest, never moves `last_used_at` backwards,
  and ignores digests revoked since they were used, so a token created again
  under a revoked name never inherits the old one's last use.
- **OAuth grants** (ADR 0022, [§7.11](7.11-oauthserver.md)) are SQLite
  `0008_oauth.sql` and Postgres `0012_oauth.sql`, additive only. Postgres uses
  `uuid` for `user_id`, `text[]` for `connectors` (`cardinality > 0`),
  `bytea` for `digest` (`octet_length(digest) = 32`), and `timestamptz`, with
  the same `CHECK` on `kind` and the same cascade. A grant's name and a scoped
  client token's share one name space: `CreateOAuthGrant` and
  `CreateClientToken` each check the other table in their transaction. Tokens
  are stored as digests only. A grant has at most one access token and one
  live refresh token; the refresh tokens it spent stay, so a reuse can be
  seen, until `PruneOAuth` drops them. How rotation and pruning work is in
  [§7.11](7.11-oauthserver.md). Deleting a grant deletes its tokens by
  cascade (SQLite's `foreign_keys` pragma is on). `last_used_at` is written by
  `TouchOAuthGrants` from the access tokens' digests, as `TouchClientTokens`
  does.
- **`calls.token_name`** names the client token a call authenticated with:
  `default`, a scoped token's name, or an OAuth grant's, which httpd verified,
  never a token value.
  `calls.client` stays the `clientInfo` name the client asserts. Rows logged
  before the migration have `token_name` NULL.
- **`calls.error_text`** (ADR 0014, amended) is the error text of a call that
  did not end `ok`, as the gateway cleaned it ([§7.6](7.6-gateway.md)): one
  line, printable, URL query strings masked, at most 500 bytes. NULL for `ok`
  and `input_required` calls and for rows logged before the migration (SQLite
  `0006_call_error_text.sql`, Postgres `0010_call_error_text.sql`). The
  retention prune deletes it with its row.
- **`calls.result_bytes`** (wayfindr-690r) is the size of the result the agent
  got back: the length of its JSON encoding, `structuredContent` included, as
  the gateway measured it ([§7.6](7.6-gateway.md)). It is metadata, never the
  result, so ADR 0014's rule that results are not stored holds. `bigint` in
  Postgres. NULL when no upstream result came back (a `rejected` or `failed`
  call), for `input_required` calls, and for rows logged before the migration;
  there is no backfill. A `tool_error` result is measured. SQLite
  `0009_call_result_bytes.sql`, Postgres `0013_call_result_bytes.sql`. The
  retention prune deletes it with its row.
- **The call log** never stores arguments or results, only the error text above.
  `connector` and `tool` are the qualified name as the agent wrote it, split at
  the first dot, clipped to 256 bytes and made printable (`redact.Printable`),
  since they come from the agent and `wayfindr calls` prints them. `result` is
  free text, not a `CHECK`, so `input_required` needed no migration. The index
  serves the newest-first listing and the retention prune ([§7.8](7.8-daemon.md)
  step 7).
- **`credentials.client_id`** records the OAuth client a grant was obtained
  with: the configured `client_id`, or the DCR client's. It binds the token to
  that client ([§7.3](7.3-auth.md), client binding). Rows from before the column
  have it NULL, which means unknown and is accepted.
- **Model identity** is `sha256(base_url ‖ model ‖ dimensions)`, hex-encoded.
  Rows whose `embedding_model` differs from the current identity are _pending_.
- **Migrations** are embedded `NNNN_name.sql` files, one directory per driver
  (`internal/store/{sqlite,postgres}/migrations`). Each runs in its own
  transaction, in order, and they are forward-only. If the database's
  `schema_version` is newer than the binary knows, the daemon refuses to start,
  which protects against downgrades. Only the version is recorded, never a
  file's content, so a migration's comments may be edited; its SQL never is.

  | SQLite                                | Postgres                                      |
  | ------------------------------------- | --------------------------------------------- |
  | `0001_init`                           | `0001_init`                                   |
  |                                       | `0002_fold_plural` ([§7.5](7.5-search.md))    |
  |                                       | `0003_unicode_search` ([§7.5](7.5-search.md)) |
  |                                       | `0004_search_index` ([§7.5](7.5-search.md))   |
  |                                       | `0005_drop_unused_search_indexes`             |
  | `0002_calls`                          | `0006_calls`                                  |
  | `0003_hidden_tools`                   | `0007_hidden_tools`                           |
  | `0004_client_tokens` (+ `token_name`) | `0008_client_tokens` (+ `token_name`)         |
  | `0005_credential_client_id`           | `0009_credential_client_id`                   |
  | `0006_call_error_text`                | `0010_call_error_text`                        |
  | `0007_client_token_remote`            | `0011_client_token_remote`                    |
  | `0008_oauth`                          | `0012_oauth`                                  |
  | `0009_call_result_bytes`              | `0013_call_result_bytes`                      |

- **SQLite settings.** WAL, `busy_timeout=5000`, `foreign_keys=ON`. The daemon
  is the only writer ([§9](9-security.md)).
- **Postgres schema.** `vector` columns carry no dimension, since it varies by
  model. `tools` also has generated `qualified_name`, `title`, and `search_text`
  columns for search ([§7.5](7.5-search.md)), computed by SQL functions in the
  migration; a definition `jsonb` cannot parse (a `\u0000` escape) only loses
  its search text. Name ordering uses the `"C"` collation, matching SQLite.
  Search reads four index tables derived from `search_text` (migration 0004,
  [§7.5](7.5-search.md)): `search_docs`, `search_postings`, `search_vocab`, and
  `search_vocab_grams`. Triggers on `tools` maintain them in the writing
  transaction; the first two cascade from `tools` on delete. `tools` has no
  index beyond its primary key. Migration 0001 created two GIN indexes,
  `tools_search_text` on `search_text` and `tools_name_trgm` on `qualified_name`
  and `title` (`gin_trgm_ops`), for `@@` and `pg_trgm` matching, which search
  never used; they only cost maintenance on every catalog write, and migration
  0005 drops them. The `pg_trgm` extension that 0001 creates stays, since other
  objects in the database may use it.
