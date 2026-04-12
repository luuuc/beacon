-- 001 — initial schema.
--
-- Divergence from .doc/definition/03-data-model.md: fingerprint is NOT NULL
-- DEFAULT '' on both tables (not nullable) because the beacon_metrics unique
-- index is a plain column list, and PostgreSQL's traditional "NULLs are
-- distinct" rule would let multiple rows with NULL fingerprint share a key
-- otherwise. The empty-string sentinel matches the Go domain type
-- (beacondb.Event.Fingerprint is a string, not a *string) and keeps the
-- partial index on beacon_events well-defined (WHERE fingerprint <> '').

CREATE TABLE IF NOT EXISTS beacon_events (
  id          BIGSERIAL PRIMARY KEY,
  kind        VARCHAR(16)  NOT NULL,
  name        VARCHAR(255) NOT NULL,
  actor_type  VARCHAR(64)  NOT NULL DEFAULT '',
  actor_id    BIGINT       NOT NULL DEFAULT 0,
  duration_ms INTEGER,
  status      INTEGER,
  fingerprint VARCHAR(64)  NOT NULL DEFAULT '',
  properties  JSONB        NOT NULL DEFAULT '{}',
  context     JSONB        NOT NULL DEFAULT '{}',
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_beacon_events_kind_name_created
  ON beacon_events (kind, name, created_at);

CREATE INDEX IF NOT EXISTS idx_beacon_events_fingerprint
  ON beacon_events (fingerprint) WHERE fingerprint <> '';

CREATE TABLE IF NOT EXISTS beacon_metrics (
  id             BIGSERIAL PRIMARY KEY,
  kind           VARCHAR(16)  NOT NULL,
  name           VARCHAR(255) NOT NULL,
  period_kind    VARCHAR(16)  NOT NULL,
  period_window  VARCHAR(8)   NOT NULL,
  period_start   TIMESTAMPTZ  NOT NULL,
  count          BIGINT       NOT NULL DEFAULT 0,
  sum            DOUBLE PRECISION,
  p50            DOUBLE PRECISION,
  p95            DOUBLE PRECISION,
  p99            DOUBLE PRECISION,
  fingerprint    VARCHAR(64)  NOT NULL DEFAULT '',
  dimensions     JSONB        NOT NULL DEFAULT '{}',
  dimension_hash CHAR(64)     NOT NULL DEFAULT '',
  created_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_beacon_metrics_unique
  ON beacon_metrics (kind, name, period_kind, period_window, period_start, fingerprint, dimension_hash);

CREATE INDEX IF NOT EXISTS idx_beacon_metrics_lookup
  ON beacon_metrics (kind, name, period_kind, period_start);
