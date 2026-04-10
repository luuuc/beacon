-- 001 — initial schema (SQLite dialect).
--
-- Timestamps are stored as INTEGER unix nanoseconds (UTC). This avoids the
-- ambiguity of SQLite's multiple timestamp text formats and gives portable
-- sub-millisecond precision across every client.
--
-- Fingerprint is NOT NULL DEFAULT '' for the same reason as the PG adapter:
-- the beacon_metrics unique index is a plain column list and needs a stable
-- non-null key. The partial index on beacon_events filters with <> ''.

CREATE TABLE IF NOT EXISTS beacon_events (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  actor_type  TEXT    NOT NULL DEFAULT '',
  actor_id    INTEGER NOT NULL DEFAULT 0,
  duration_ms INTEGER,
  status      INTEGER,
  fingerprint TEXT    NOT NULL DEFAULT '',
  properties  TEXT    NOT NULL DEFAULT '{}',
  context     TEXT    NOT NULL DEFAULT '{}',
  created_at  INTEGER NOT NULL                   -- unix nanoseconds
);

CREATE INDEX IF NOT EXISTS idx_beacon_events_kind_name_created
  ON beacon_events (kind, name, created_at);

CREATE INDEX IF NOT EXISTS idx_beacon_events_fingerprint
  ON beacon_events (fingerprint) WHERE fingerprint <> '';

CREATE TABLE IF NOT EXISTS beacon_metrics (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  kind           TEXT    NOT NULL,
  name           TEXT    NOT NULL,
  period_kind    TEXT    NOT NULL,
  period_window  TEXT    NOT NULL,
  period_start   INTEGER NOT NULL,               -- unix nanoseconds
  count          INTEGER NOT NULL DEFAULT 0,
  sum            REAL,
  p50            REAL,
  p95            REAL,
  p99            REAL,
  fingerprint    TEXT    NOT NULL DEFAULT '',
  dimensions     TEXT    NOT NULL DEFAULT '{}',
  dimension_hash TEXT    NOT NULL DEFAULT '',
  created_at     INTEGER NOT NULL,
  updated_at     INTEGER NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_beacon_metrics_unique
  ON beacon_metrics (kind, name, period_kind, period_window, period_start, fingerprint, dimension_hash);

CREATE INDEX IF NOT EXISTS idx_beacon_metrics_lookup
  ON beacon_metrics (kind, name, period_kind, period_start);
