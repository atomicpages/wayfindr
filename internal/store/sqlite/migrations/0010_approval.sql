-- Approval entries of a scoped client token and of an OAuth grant (ADR 0024):
-- calls matching them wait for the owner's approval. A JSON array of
-- strings, stored as given and parsed by the gateway. Set when the token or
-- grant is created and never changed. NULL means none, which is every row
-- made before this migration.

ALTER TABLE client_tokens ADD COLUMN approval TEXT;
ALTER TABLE oauth_grants ADD COLUMN approval TEXT;
