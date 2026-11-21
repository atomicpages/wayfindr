-- The OAuth client_id a credential's grant was obtained with, manual or DCR.
-- NULL for rows from before this column: the client is unknown.

ALTER TABLE credentials ADD COLUMN client_id TEXT;
