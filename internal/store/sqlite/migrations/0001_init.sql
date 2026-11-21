-- Initial schema (docs/internal/design.md §5). schema_version is owned by the
-- migration runner in internal/store. Timestamps are Unix milliseconds.

CREATE TABLE users (
  id         TEXT PRIMARY KEY,          -- UUIDv7
  created_at INTEGER NOT NULL
);

CREATE TABLE tools (
  user_id         TEXT NOT NULL REFERENCES users(id),
  connector       TEXT NOT NULL,
  name            TEXT NOT NULL,        -- upstream (unqualified) tool name
  definition      BLOB NOT NULL,        -- mcp.Tool JSON exactly as received
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
  PRIMARY KEY (user_id, connector)
);

CREATE TABLE credentials (
  user_id          TEXT NOT NULL,
  connector        TEXT NOT NULL,
  resource         TEXT NOT NULL,       -- canonical MCP URL the tokens are bound to (RFC 8707)
  issuer           TEXT,                -- authorization server issuer
  redirect_uri     TEXT,                -- URI the DCR client was registered with
  dcr_client_id    TEXT,                -- set only when obtained via DCR
  dcr_client       BLOB,                -- sealed: DCR client_secret + registration response
  token            BLOB,                -- sealed: {access, refresh, type, expiry}
  granted_scopes   TEXT,
  requested_scopes TEXT,                -- includes step-up additions
  updated_at       INTEGER NOT NULL,
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
