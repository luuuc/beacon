-- 002 — widen actor_id from BIGINT to VARCHAR(128).
--
-- v1 shipped with actor_id BIGINT NOT NULL DEFAULT 0, which silently
-- assumed every application used integer primary keys for their users.
-- Modern Rails apps (Rails 7.1+ default on Postgres, universal in Rails
-- 8.1) use UUIDs, and the Beacon Ruby client was handing UUIDs to a
-- BIGINT column, producing a 400 on every Beacon.track call with a
-- `user:` argument.
--
-- This migration widens the column to VARCHAR(128), which is wider than
-- UUID (36), ULID (26), Snowflake (19), or any realistic ID format. The
-- previous "no actor" sentinel (BIGINT 0) becomes the new sentinel
-- (empty string); the Go envelope validator treats '' as "no actor."

ALTER TABLE beacon_events
  ALTER COLUMN actor_id DROP DEFAULT;

ALTER TABLE beacon_events
  ALTER COLUMN actor_id TYPE VARCHAR(128)
    USING CASE WHEN actor_id = 0 THEN '' ELSE actor_id::text END;

ALTER TABLE beacon_events
  ALTER COLUMN actor_id SET DEFAULT '';
