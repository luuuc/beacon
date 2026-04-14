# Pitch 01a — Noise filtering and anomaly curation

*Make Beacon's data trustworthy by killing noise at the source and making anomalies actionable. Follow-up to pitch 01, cards 3 (anomaly detector), 4 (anomaly read endpoint + MCP tool), and 7 (anomaly dashboard page).*

**Appetite:** small batch (~3 days)
**Status:** Building
**Owner:** Solo founder + AI producers
**Predecessor:** `pitches/01-ambient-anomalies.md` (shipped the anomaly detector, `beacon.anomalies` MCP tool, and `/anomalies` dashboard page)
**Related:** `definition/06-http-api.md` (MCP server contract, `/api/anomalies` endpoint), `definition/03-data-model.md` (anomaly records in `beacon_metrics`), `decisions/0002-maket-first-integration.md` (Maket as proof point)

---

## Problem

Pitch 01 shipped ambient anomaly detection — a sigma-threshold detector that watches for volume shifts and dimension spikes, surfaced via `GET /api/anomalies`, the `beacon.anomalies` MCP tool, and a dashboard page. The detector works. The problem is that its output is polluted by noise and unactionable in practice. Both problems showed up within the first week of Maket production data.

**The noise problem:** `beacon.perf_drift` returns 250+ endpoints. Roughly 150 of them are WordPress vulnerability probes — `/wp-class.php`, `/sadcut1.php`, `/lock360.php`, `/666.php`. They all have `drift_sigmas: 0` and zero real traffic. A user opening the performance page sees garbage drowning real signals. An agent querying `beacon.perf_drift` gets a wall of irrelevant data before reaching the actual endpoints.

The same probes pollute anomalies. `beacon.anomalies` over 7 days returns 34 entries. Many are dimension spikes on bot-probed paths, health check endpoints (`GET /up`), and recurring jobs whose count fluctuates normally. The user has to mentally filter 80% of the list to find anything worth investigating.

This is not a display problem — it's a data hygiene problem. The Ruby client captures every HTTP request indiscriminately. That's correct behavior for a middleware, but Beacon needs to drop known noise before it enters the database.

**The anomaly actionability problem:** Even after noise is removed, the anomaly surface doesn't help you act. Every anomaly summary reads "X dimension_spike: 49 in 24h vs baseline of 4" — mechanical, undifferentiated, and impossible to triage at scale. There's no way to dismiss an expected anomaly (traffic spike after a marketing email), no way to distinguish "this is concerning" from "this is Tuesday," and no acknowledgment workflow so the same anomaly doesn't keep showing up.

---

## Appetite

**Small batch — ~3 days.** Bot filtering is hours of work that transforms the entire product. Anomaly dismiss is a small state addition. Smarter summaries are a formatting pass. The scope is deliberately tight: no severity tiers, no alert routing, no machine learning. Clean the data, let the user dismiss what they've seen, and write summaries a human can scan.

---

## Solution

### Path exclusion filter

A new `filter` block in `beacon.yml` (and corresponding env var):

```yaml
filter:
  exclude_paths:
    - "*.php"
    - "/wp-*"
    - "/wp-content/*"
    - "/wp-admin/*"
    - "/wp-includes/*"
    - "/cgi-bin*"
```

Applied at the **ingest layer** — events matching excluded path patterns are dropped in the `POST /api/events` handler before writing to `beacon_events`. They generate no raw event row, no rollup, and no anomaly record. On a heavy-traffic app, this keeps the database clean and queries fast. Dropped events are counted via an atomic counter exposed in `/api/stats` (and the active filter patterns are logged at startup) so the operator has visibility into what's being filtered and how aggressively.

The filter uses glob matching (Go's `path.Match`). The patterns match against the path portion of perf event names (e.g., the `/wp-class.php` in `GET /wp-class.php`). Non-perf events are not filtered by path.

**Default exclusions:** Beacon ships with a built-in set of common scanner patterns that always apply. User-configured `filter.exclude_paths` patterns are **additive** — they extend the defaults. To disable the built-in defaults entirely, set `filter.defaults: false`.

The built-in list covers: `*.php`, `/wp-*`, `/cgi-bin*`, `/xmlrpc*`, common CMS probe paths.

### Anomaly dismiss

A new `dismissed_at` timestamp column on anomaly records in `beacon_metrics`. Dashboard-only — no MCP tool. MCP stays read-only.

The dismiss is per-anomaly-record, not per-metric. If the same metric spikes again tomorrow, that's a new anomaly record and it shows up fresh.

Dashboard: a small "×" button on each anomaly card. Click dismisses via an htmx `DELETE` to `/anomalies/:id/dismiss`. The card fades out. `GetAnomalies` excludes dismissed records.

A corresponding `DELETE /api/anomalies/:id` HTTP endpoint exists for programmatic access (e.g., if Patrol or a script needs to dismiss), but it's not exposed as an MCP tool. MCP remains read-only.

### Smarter anomaly summaries

Replace the mechanical template ("X dimension_spike: 49 in 24h vs baseline of 4") with summaries that tell the user what changed and by how much in human terms:

- **Volume shift:** "GET /search saw 3× normal traffic (240 vs ~80/day)" instead of "GET /search volume_shift: 240 in 24h vs baseline of 80"
- **Dimension spike:** "GET /items/:id from country=DE jumped to 47 (normally ~3/day)" instead of "GET /items/:id dimension_spike: 47 in 24h vs baseline of 3"
- **Include the σ as parenthetical context**, not the headline: "(12σ above baseline)" at the end, not "deviation_sigma: 12"

The summary is generated at anomaly-detection time and stored in the `summary` field (which already exists). No new column needed — just better text.

### Raise anomaly floor to 3σ

Recurring jobs (SolidQueue cron jobs, scheduled tasks) naturally fluctuate in count based on how many hours have passed in the detection window. A job that runs every 30 minutes will show 24 executions/day at midnight and 48 at midnight the next day — the detector sees a "spike" that's just wall-clock progress.

Rather than building a "recurring job" classifier, raise the default `sigma_threshold` from 2.0 to 3.0. The current default is too low — Maket production data proved it generates background hum from normal fluctuation. 3σ is the standard statistical convention for "unlikely to be chance" (99.7% confidence). This is a default change, not a separate floor — users who have explicitly set `sigma_threshold` in `beacon.yml` keep their value. No new config key.

---

## Rabbit holes

### Glob matching edge cases

`path.Match` in Go has specific semantics — `*` does not match `/`, `**` is not supported. This means `*.php` matches `/foo.php` but not `/wp-content/foo.php`. For deeper paths, the user needs explicit patterns like `/wp-content/*`. Document the glob syntax clearly. Don't build a custom glob engine — `path.Match` plus one layer of "match against each path segment" is enough.

### Default exclusion list maintenance

The built-in scanner pattern list will need occasional updates as new probe patterns emerge. Don't over-engineer this — a string slice in Go source that ships with the binary is fine. Users who need custom patterns configure `filter.exclude_paths`. The defaults are a convenience, not a security feature.

### Existing noisy data in the database

After deploying ingest-layer filtering, the 150+ scanner endpoints already in `beacon_metrics` from before the filter was enabled won't disappear on their own. Two options: (a) let them age out naturally as rollup retention expires, or (b) add a one-time cleanup command or migration. Option (a) is simpler — the noisy rollups will stop being replenished and will eventually prune. If the noise is intolerable during the transition, a manual `DELETE FROM beacon_metrics WHERE name LIKE '%.php'` is acceptable as a one-time ops task, not a product feature.

---

## No-gos

- **Severity tiers.** The council debated this and the consensus is: clean the data first, then see if tiers are needed. With bot noise gone and a 3σ floor, the anomaly list may already be actionable without tiers.
- **Alert routing or notifications.** Beacon does not alert. The dismiss button is for triage, not for silencing alerts that don't exist.
- **Read-layer filtering.** Events are dropped at ingest. This is a deliberate decision for performance at scale — scanner probes never enter the database.
- **MCP write tools.** Anomaly dismiss is dashboard-only (plus an HTTP API endpoint for scripts). MCP stays read-only.
- **Machine learning for anomaly classification.** The sigma-threshold detector is the right tool. Smarter summaries are a text-generation problem, not an ML problem.
- **Per-user or per-session dismiss state.** Beacon is single-tenant. Dismiss is global.
- **Auto-dismiss rules** (e.g., "always dismiss health check anomalies"). The path filter handles the known-noise case. Auto-dismiss is a feature for when there are users asking for it.

---

## Acceptance Criteria

1. **Noise is gone from perf views.** `beacon.perf_drift` on Maket returns only real application endpoints — no `.php` probes, no `/wp-*` scanner paths. The count drops from 250+ to roughly 30-50 real endpoints.
2. **Anomaly list is actionable.** `beacon.anomalies` over 7 days on Maket returns fewer than 10 entries, each of which a human would look at and say "that's worth knowing."
3. **Dismissed anomalies stay dismissed.** Clicking dismiss on the dashboard removes the anomaly from both the dashboard and API responses. It doesn't come back on refresh or on the next query.
4. **Summaries are scannable.** A human reading the anomaly list can understand each entry without mentally parsing "dimension_spike: 49 in 24h vs baseline of 4." The new format reads like English.
5. **Filtered events never hit the database.** Events matching excluded path patterns are dropped at ingest. A `.php` probe request generates no `beacon_events` row and no `beacon_metrics` rollup.

---

## Scope

Cards are in dependency order. Cards 1-3 are P0. Cards 4-5 are P1.

- [x] **Path exclusion filter in config** — add `filter.exclude_paths` and `filter.defaults` to `beacon.yml` parsing and `Config` struct. Ship with a built-in default list (`*.php`, `/wp-*`, `/cgi-bin*`, `/xmlrpc*`). User config extends defaults; `filter.defaults: false` disables them. Glob matching via `path.Match` against the path portion of perf event names. *Done when:* a config with `exclude_paths: ["*.php"]` parses correctly, and a test confirms `path.Match` behavior against representative scanner paths.

- [x] **Apply filter at ingest** — modify the `POST /api/events` handler to drop perf events whose path matches an excluded pattern before writing to `beacon_events`. Dropped events are counted via an atomic counter exposed in `/api/stats`. Active filter patterns are logged once at startup. *Done when:* a test POSTs a batch containing both real endpoints and `.php` probe paths, and asserts only real endpoints are written to `beacon_events`. The anomaly detector and rollup worker never see filtered events. The `/api/stats` response includes `filtered_events_total` and `filter_patterns`.

- [x] **Raise anomaly detector default to 3σ** — change the `sigma_threshold` default from 2.0 to 3.0 in config defaults. No new config key — this is a default change, not a separate floor. *Done when:* the default in `rollup.go` is 3.0, a test confirms anomalies at 2.5σ are not written and anomalies at 3.5σ are (using the default config). The Maket anomaly list drops from ~34 to a manageable number.

- [ ] **Anomaly dismiss** — add `dismissed_at` column to anomaly records in `beacon_metrics`. Dashboard: "×" button on anomaly cards, htmx DELETE to `/anomalies/:id/dismiss`. HTTP API: `DELETE /api/anomalies/:id`. `GetAnomalies` excludes dismissed records. *Done when:* dismissing an anomaly in the dashboard removes it from the list, a dismissed anomaly stays gone across page refreshes and API queries, and `DELETE /api/anomalies/:id` returns 200 with subsequent `GET /api/anomalies` excluding it.

- [ ] **Smarter anomaly summaries** — rewrite the summary template in the anomaly detector to produce human-scannable text. Volume shifts: "X saw N× normal traffic (current vs ~baseline/day)". Dimension spikes: "X from dimension=value jumped to N (normally ~baseline/day)". σ as parenthetical. *Done when:* new summaries appear in both dashboard and MCP responses, and a test asserts the new format against a known anomaly record.
