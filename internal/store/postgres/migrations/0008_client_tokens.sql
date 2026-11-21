-- Scoped client tokens, each limited to its connectors
-- (docs/internal/plans/scoped-client-tokens.md). Only a token's SHA-256 is
-- stored, never its value. The digest is unique across users, so a future
-- team mode still maps one value to one user.
--
-- calls.token_name names the client token a call authenticated with: 'default' or a
-- scoped token's name; NULL for calls logged before this migration.
-- calls.client stays the clientInfo name the client asserts.

CREATE TABLE client_tokens (
  user_id      uuid NOT NULL REFERENCES users(id),
  name         text NOT NULL,                  -- connector-name rule; never 'default'
  digest       bytea NOT NULL UNIQUE,          -- SHA-256 of the token
  connectors   text[] NOT NULL,                -- at least one
  created_at   timestamptz NOT NULL DEFAULT now(),
  last_used_at timestamptz,                    -- NULL until first use
  PRIMARY KEY (user_id, name),
  CHECK (octet_length(digest) = 32),
  CHECK (cardinality(connectors) > 0)
);

ALTER TABLE calls ADD COLUMN token_name text;
