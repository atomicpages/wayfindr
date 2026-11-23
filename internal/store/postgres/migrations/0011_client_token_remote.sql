-- Whether a scoped client token may reach /mcp through a tunnel (ADR 0021).
-- Set when the token is created and never changed. false for every token
-- made before this migration, which were all made for a local client.

ALTER TABLE client_tokens ADD COLUMN remote boolean NOT NULL DEFAULT false;
