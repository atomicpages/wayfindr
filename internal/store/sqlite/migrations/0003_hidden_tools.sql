-- The tools a connector's tool policy kept out of the catalog at its last
-- successful sync, as a JSON array of {name, class}
-- (docs/internal/plans/tool-policy.md). NULL before the first one.

ALTER TABLE connector_sync ADD COLUMN hidden_tools TEXT;
