-- The call log: one row per call_tool call (docs/internal/plans/call-log.md).
-- Arguments and results are never stored. Timestamps are Unix milliseconds.

CREATE TABLE calls (
  id          TEXT PRIMARY KEY,         -- UUIDv7
  user_id     TEXT NOT NULL,
  client      TEXT,                     -- calling client; NULL when unknown
  connector   TEXT NOT NULL DEFAULT '', -- as the agent wrote it
  tool        TEXT NOT NULL DEFAULT '', -- as the agent wrote it
  started_at  INTEGER NOT NULL,
  duration_ms INTEGER,                  -- NULL when the upstream was not called
  result      TEXT NOT NULL,            -- 'ok' | 'tool_error' | 'rejected' | 'failed'
  error_kind  TEXT,
  error_case  TEXT
);

CREATE INDEX calls_user_started ON calls (user_id, started_at DESC, id DESC);
