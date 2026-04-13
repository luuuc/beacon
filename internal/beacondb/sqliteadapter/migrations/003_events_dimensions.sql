-- 003 — add dimensions column to beacon_events.
--
-- See the pgadapter version for the rationale. SQLite stores JSON as TEXT.

ALTER TABLE beacon_events
  ADD COLUMN dimensions TEXT NOT NULL DEFAULT '{}';
