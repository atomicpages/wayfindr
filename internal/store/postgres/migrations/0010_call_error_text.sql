-- The error text the agent got back from a call that did not end ok,
-- cleaned and clipped by the gateway (ADR 0014, amended). NULL for ok and
-- input_required calls, and for rows from before this column.

ALTER TABLE calls ADD COLUMN error_text text;
