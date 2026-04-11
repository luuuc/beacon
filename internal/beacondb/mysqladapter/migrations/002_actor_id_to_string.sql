-- 002 — widen actor_id from BIGINT to VARCHAR(128).
--
-- See the pgadapter version for the rationale. MySQL's MODIFY COLUMN
-- converts existing BIGINT values to their string form, so the sentinel
-- 0 becomes "0" on conversion — we then UPDATE it to "" to match the
-- new "empty string = no actor" convention. Run inside a transaction
-- by the migration driver, so the MODIFY and the UPDATE are atomic.

ALTER TABLE beacon_events
  MODIFY COLUMN actor_id VARCHAR(128) NOT NULL DEFAULT '';

UPDATE beacon_events SET actor_id = '' WHERE actor_id = '0';
