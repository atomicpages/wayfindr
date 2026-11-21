-- Scoped client tokens, each limited to its connectors
-- (docs/internal/plans/scoped-client-tokens.md). Only a token's SHA-256 is
-- stored, never its value. Timestamps are Unix milliseconds.
--
-- calls.token_name names the client token a call authenticated with: 'default' or a
-- scoped token's name; NULL for calls logged before this migration.
-- calls.client stays the clientInfo name the client asserts.

CREATE TABLE client_tokens (
  user_id      TEXT NOT NULL REFERENCES users(id),
  name         TEXT NOT NULL,             -- connector-name rule; never 'default'
  digest       BLOB NOT NULL UNIQUE,      -- SHA-256 of the token, 32 bytes
  connectors   TEXT NOT NULL,             -- JSON array of connector names, at least one
  created_at   INTEGER NOT NULL,
  last_used_at INTEGER,                   -- NULL until first use
  PRIMARY KEY (user_id, name)
);

ALTER TABLE calls ADD COLUMN token_name TEXT;
