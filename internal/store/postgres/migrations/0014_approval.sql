-- Approval entries of a scoped client token and of an OAuth grant (ADR 0024):
-- calls matching them wait for the owner's approval. Stored as given and
-- parsed by the gateway. Set when the token or grant is created and never
-- changed. NULL means none, which is every row made before this migration.

ALTER TABLE client_tokens ADD COLUMN approval text[];
ALTER TABLE oauth_grants ADD COLUMN approval text[];
