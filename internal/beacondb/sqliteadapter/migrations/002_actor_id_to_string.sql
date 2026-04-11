-- 002 — widen actor_id from INTEGER to TEXT.
--
-- SQLite does not support ALTER COLUMN TYPE directly, so we use the
-- table-swap dance: create a new table with the desired schema, copy
-- the old rows in (converting the integer sentinel 0 to the empty
-- string), drop the old table, rename the new one into place, and
-- recreate indexes. The whole block runs inside the migration's
-- transaction, so failure rolls everything back.
--
-- See the pgadapter version for the rationale (Rails UUID users).

CREATE TABLE beacon_events_new (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  actor_type  TEXT    NOT NULL DEFAULT '',
  actor_id    TEXT    NOT NULL DEFAULT '',
  duration_ms INTEGER,
  status      INTEGER,
  fingerprint TEXT    NOT NULL DEFAULT '',
  properties  TEXT    NOT NULL DEFAULT '{}',
  context     TEXT    NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL
);

INSERT INTO beacon_events_new
  (id, kind, name, actor_type, actor_id, duration_ms, status, fingerprint, properties, context, created_at)
SELECT
  id, kind, name, actor_type,
  CASE WHEN actor_id = 0 THEN '' ELSE CAST(actor_id AS TEXT) END,
  duration_ms, status, fingerprint, properties, context, created_at
FROM beacon_events;

DROP TABLE beacon_events;

ALTER TABLE beacon_events_new RENAME TO beacon_events;

CREATE INDEX idx_beacon_events_kind_name_created
  ON beacon_events (kind, name, created_at);

CREATE INDEX idx_beacon_events_fingerprint
  ON beacon_events (fingerprint) WHERE fingerprint <> '';
