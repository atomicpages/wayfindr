-- The size in bytes of the result the agent got back from a call: the
-- length of its JSON encoding, content and structuredContent included
-- (design §7.6). NULL when no upstream result came back (a refusal, a
-- failure), for input_required calls, and for rows from before this column.

ALTER TABLE calls ADD COLUMN result_bytes INTEGER;
