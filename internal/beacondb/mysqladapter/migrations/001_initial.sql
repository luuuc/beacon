-- 001 — initial schema (MySQL / MariaDB dialect).
--
-- Storage model deltas from the Postgres adapter:
--
--   * Timestamps are BIGINT unix nanoseconds (UTC). MySQL's DATETIME/TIMESTAMP
--     types have timezone quirks and microsecond-precision edge cases; beacon
--     owns the schema and never needs native SQL time ops, so nanoseconds are
--     the honest choice (matching sqliteadapter).
--   * JSONB is JSON. No default values — Go always binds.
--   * Table COLLATE=utf8mb4_bin so the seven-tuple unique index is
--     case-sensitive and byte-exact (default utf8mb4 collations fold case
--     and would merge rows that should be distinct).
--   * Partial indexes are emulated by a plain index on fingerprint. The card
--     explicitly permits this emulation; the selectivity cost is acceptable
--     because beacon is never the write-hot table in its host database.

CREATE TABLE IF NOT EXISTS beacon_events (
  id          BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  kind        VARCHAR(16)  NOT NULL,
  name        VARCHAR(255) NOT NULL,
  actor_type  VARCHAR(64)  NOT NULL DEFAULT '',
  actor_id    BIGINT       NOT NULL DEFAULT 0,
  duration_ms INT          NULL,
  status      INT          NULL,
  fingerprint VARCHAR(64)  NOT NULL DEFAULT '',
  properties  JSON         NOT NULL,
  context     JSON         NOT NULL,
  created_at  BIGINT       NOT NULL,
  KEY idx_beacon_events_kind_name_created (kind, name, created_at),
  KEY idx_beacon_events_fingerprint (fingerprint)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;

CREATE TABLE IF NOT EXISTS beacon_metrics (
  id             BIGINT       NOT NULL AUTO_INCREMENT PRIMARY KEY,
  kind           VARCHAR(16)  NOT NULL,
  name           VARCHAR(255) NOT NULL,
  period_kind    VARCHAR(16)  NOT NULL,
  period_window  VARCHAR(8)   NOT NULL,
  period_start   BIGINT       NOT NULL,
  count          BIGINT       NOT NULL DEFAULT 0,
  sum            DOUBLE       NULL,
  p50            DOUBLE       NULL,
  p95            DOUBLE       NULL,
  p99            DOUBLE       NULL,
  fingerprint    VARCHAR(64)  NOT NULL DEFAULT '',
  dimensions     JSON         NOT NULL,
  dimension_hash CHAR(64)     NOT NULL DEFAULT '',
  created_at     BIGINT       NOT NULL,
  updated_at     BIGINT       NOT NULL,
  UNIQUE KEY idx_beacon_metrics_unique
    (kind, name, period_kind, period_window, period_start, fingerprint, dimension_hash),
  KEY idx_beacon_metrics_lookup (kind, name, period_kind, period_start)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
