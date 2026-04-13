-- 003 — add dimensions column to beacon_events.
--
-- See the pgadapter version for the rationale. MySQL uses JSON (not JSONB).
-- DEFAULT (CAST('{}' AS JSON)) is required because the column is NOT NULL
-- and existing rows need a value.

ALTER TABLE beacon_events
  ADD COLUMN dimensions JSON NOT NULL DEFAULT (CAST('{}' AS JSON));
