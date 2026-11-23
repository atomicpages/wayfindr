-- OAuth grants and their tokens (ADR 0022, docs/internal/design/7.11-oauthserver.md).
-- A grant's name shares a name space with client_tokens.name: the store
-- refuses a name the other table has, in the inserting transaction. Only a
-- token's SHA-256 is stored, never its value. Additive only: a rollback
-- leaves both tables unused.

CREATE TABLE oauth_grants (
  user_id      uuid NOT NULL REFERENCES users(id),
  name         text NOT NULL,                  -- connector-name rule; never 'default'; no client_tokens row has it
  client_id    text NOT NULL,                  -- the allowed client's CIMD URL
  resource     text NOT NULL,                  -- <public_url>/mcp when approved (RFC 8707)
  connectors   text[] NOT NULL,                -- at least one
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,                    -- NULL until its access token is first used
  PRIMARY KEY (user_id, name),
  CHECK (cardinality(connectors) > 0)
);

CREATE TABLE oauth_tokens (
  digest       bytea PRIMARY KEY,              -- SHA-256 of the token
  user_id      uuid NOT NULL,
  grant_name   text NOT NULL,                  -- `grant` is an SQL keyword
  kind         text NOT NULL CHECK (kind IN ('access', 'refresh')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz,                    -- access: set; refresh: NULL (no expiry)
  used_at      timestamptz,                    -- refresh: when it was spent; NULL until then
  CHECK (octet_length(digest) = 32),
  FOREIGN KEY (user_id, grant_name) REFERENCES oauth_grants (user_id, name) ON DELETE CASCADE
);

CREATE INDEX oauth_tokens_grant ON oauth_tokens (user_id, grant_name);
