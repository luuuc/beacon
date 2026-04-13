-- 003 — add dimensions column to beacon_events.
--
-- Enrichment dimensions (country, plan, locale) provided by the client's
-- enrich_context hook are stored separately from properties so the rollup
-- worker can bucket events by dimension without parsing properties.
-- Defaults to '{}' (empty object) for existing and unenriched events.

ALTER TABLE beacon_events
  ADD COLUMN IF NOT EXISTS dimensions JSONB NOT NULL DEFAULT '{}';
