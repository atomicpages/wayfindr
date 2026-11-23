-- Whether a scoped client token may reach /mcp through a tunnel (ADR 0021).
-- Set when the token is created and never changed. 0 for every token made
-- before this migration, which were all made for a local client.

ALTER TABLE client_tokens ADD COLUMN remote INTEGER NOT NULL DEFAULT 0 CHECK (remote IN (0, 1));
