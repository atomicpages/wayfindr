-- OAuth grants and their tokens (ADR 0022, docs/internal/design/7.11-oauthserver.md).
-- A grant's name shares a name space with client_tokens.name: the store
-- refuses a name the other table has, in the inserting transaction. Only a
-- token's SHA-256 is stored, never its value. Timestamps are Unix
-- milliseconds. Additive only: a rollback leaves both tables unused.

CREATE TABLE oauth_grants (
  user_id      TEXT NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,             -- connector-name rule; never 'default'; no client_tokens row has it
  client_id    TEXT NOT NULL,             -- the allowed client's CIMD URL
  resource     TEXT NOT NULL,             -- <public_url>/mcp when approved (RFC 8707)
  connectors   TEXT NOT NULL,             -- JSON array of connector names, at least one
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,                   -- NULL until its access token is first used
  PRIMARY KEY (user_id, name)
);

CREATE TABLE oauth_tokens (
  digest       BLOB PRIMARY KEY CHECK (length(digest) = 32), -- SHA-256 of the token
  user_id      TEXT NOT NULL,
  grant_name   TEXT NOT NULL,             -- `grant` is an SQL keyword
  kind         TEXT NOT NULL CHECK (kind IN ('access', 'refresh')),
  created_at   INTEGER NOT NULL,
  expires_at   INTEGER,                   -- access: set; refresh: NULL (no expiry)
  used_at      INTEGER,                   -- refresh: when it was spent; NULL until then
  FOREIGN KEY (user_id, grant_name) REFERENCES oauth_grants (user_id, name) ON DELETE CASCADE
);

CREATE INDEX oauth_tokens_grant ON oauth_tokens (user_id, grant_name);
