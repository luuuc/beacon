# Pitch 01b — Errors depth

*Make error detail actually help a human or agent investigate. Follow-up to pitch 00 (error fingerprinting, cards 4-6), pitch 00d (error detail dashboard page, card 7), and pitch 01 (anomaly detection revealed the gap during Maket production testing).*

**Appetite:** small batch (~3-5 days)
**Status:** Shipped
**Owner:** Solo founder + AI producers
**Predecessor:** `pitches/00-bootstrap.md` (cards 4-6: error capture, fingerprinting, `beacon.errors` MCP tool), `pitches/00d-dashboard.md` (card 7: errors pillar page + detail), `pitches/01-ambient-anomalies.md` (Maket production rollout revealed the investigation gap)
**Related:** `definition/06-http-api.md` (`GET /api/errors` endpoint, fingerprint algorithm, MCP tools), `definition/03-data-model.md` (error events schema, `properties.stack_trace`, `context.deploy_sha`), `definition/08-ai-workflow.md` (agents cross-referencing error fingerprints against diffs — currently blocked by missing MCP tool)

---

## Problem

Pitch 00 shipped error capture: the Ruby client fingerprints exceptions (SHA1 of class + first app frame), the server rolls them up, and `beacon.errors` lists active fingerprints. Pitch 00d added a dashboard error detail page with a sample stack trace and hourly chart. These surfaces tell you *what* is breaking. They don't help you figure out *why*.

Two concrete failures from Maket production:

1. **The MCP has no way to investigate an error.** `beacon.errors` returns `name + fingerprint + first_seen + last_seen + occurrences`. An agent sees "ActionView::Template::Error, 3 occurrences" and hits a wall. There's no MCP tool to fetch the stack trace, no request context, no way to know what the user was doing. The AI workflow doc (`08-ai-workflow.md`) describes agents cross-referencing error fingerprints against diffs — but they can't, because the detail isn't exposed.

2. **The dashboard error detail page shows a stack trace in isolation.** You get a `<pre>` block of the stack trace, an hourly occurrence chart, and timestamps. You don't know: which endpoint triggered it, what the request parameters were, which deploy introduced it, or whether it's getting worse or stabilizing. Sentry, Honeybadger, and Bugsnag all show request context alongside the trace — it's table stakes for error investigation.

The raw data exists. `beacon_events` stores `context` (request_id, deploy_sha, environment) and `properties` (fingerprint, message, first_app_frame, stack_trace). The error detail page fetches one sample event but only renders `stack_trace`. Everything else in the event is ignored.

---

## Appetite

**Small batch — 3 to 5 days.** The data is already in the database. The work is: a new MCP tool, richer dashboard rendering of existing fields, and a persistent deploy-correlation field. No new data ingestion, no new client instrumentation. One small schema addition (`introduced_deploy_sha` on `beacon_metrics`).

---

## Solution

### MCP: `beacon.error_detail` tool

A new MCP tool that takes a fingerprint and returns the full investigation payload:

```json
{
  "fingerprint": "e112b0bcb36265b97e8e32fafcc7ab39abe834d5",
  "name": "ActionView::Template::Error",
  "message": "undefined method `title' for nil:NilClass",
  "first_seen": "2026-04-13T09:00:00Z",
  "last_seen": "2026-04-13T19:00:00Z",
  "occurrences": 3,
  "introduced_deploy_sha": "9f3a2c1",
  "first_app_frame": "app/views/listings/show.html.erb:42",
  "stack_trace": "...",
  "sample_context": {
    "request_id": "abc-123",
    "deploy_sha": "b4f1e5f",
    "environment": "production"
  },
  "sample_properties": {
    "path": "GET /items/47",
    "status": 500
  },
  "hourly_occurrences": [
    { "period_start": "2026-04-13T09:00:00Z", "count": 1 },
    { "period_start": "2026-04-13T11:00:00Z", "count": 1 },
    { "period_start": "2026-04-13T19:00:00Z", "count": 1 }
  ]
}
```

This gives an agent everything it needs to investigate: the error message, the stack trace, the request that triggered it, the deploy that introduced it, and the occurrence pattern. An agent can now cross-reference the fingerprint against its diff, check if the deploy SHA matches the change it just shipped, and decide whether to fix or note the error.

The data comes from two sources: the most recent `beacon_events` row matching the fingerprint (for properties and context — one sample only), and the hourly rollups in `beacon_metrics` (for the occurrence timeline and `introduced_deploy_sha`).

### Dashboard: enriched error detail page

The existing error detail page (`/errors/:fingerprint`) already fetches a sample event. Extend the rendering to show:

**Request context block** (new, above the stack trace):
- Request path and method (from sample event properties)
- Deploy SHA (from sample event context)
- Environment
- Request ID

**Error summary bar** (enhanced):
- Exception name + message (currently only name is shown in the heading)
- First app frame (clickable path for copy-paste into editor)
- Introduced in deploy: `<sha>` (from persisted `introduced_deploy_sha`)
- First seen / Last seen / Occurrences (already exists)
- Trend indicator: are occurrences increasing, stable, or decreasing over the last 24h?

**Stack trace** (enhanced):
- App frames highlighted vs framework/gem frames (app frames in bold or different color, framework frames dimmed)
- Hardcoded heuristic: frames containing `/vendor/`, `/gems/`, `/ruby/`, `/lib/ruby/` are framework; everything else is app
- First app frame visually marked as the probable origin

**Occurrence timeline** (already exists as hourly chart — no change needed)

### Read layer: `GetErrorDetail` handler

A new method on `internal/reads` that takes a fingerprint and returns a structured error detail. Combines:
1. The error summary from rollup data (name, first_seen, last_seen, occurrences, `introduced_deploy_sha`)
2. The most recent raw event matching the fingerprint (for stack_trace, message, context, properties) — one sample only
3. Hourly occurrence data from `beacon_metrics`

This handler backs both the MCP tool and the dashboard detail page. One query path, two consumers.

When the sample event has been pruned (>14 days), the handler returns rollup data and `introduced_deploy_sha` (which survives pruning), with `sample_event: null`. The dashboard shows a "Sample event pruned" notice in place of the stack trace and request context blocks.

### Deploy correlation — persisted on metrics row

A new nullable `introduced_deploy_sha VARCHAR(64)` column on `beacon_metrics` for error-kind rows. Set once: when the rollup worker creates the first metric row for a new fingerprint, it copies `context.deploy_sha` from the triggering event. Never overwritten.

This survives raw event pruning indefinitely. An error first seen 90 days ago still shows which deploy introduced it. No scan of `beacon_events` needed at read time — it's just a field on the metrics row.

---

## Rabbit holes

### Raw event retention and error detail

Error events are retained for 14 days. After that, the sample stack trace and request context are gone — only the rollup counts and `introduced_deploy_sha` remain. The error detail page/MCP tool degrades gracefully: show the rollup data and indicate that the sample event has been pruned. Don't try to extend retention for errors — that's a schema/retention decision for a future pitch.

### App frame highlighting heuristics

The hardcoded heuristic (frames containing `/vendor/`, `/gems/`, `/ruby/`, `/lib/ruby/` are framework) is correct for Ruby/Rails apps. It won't work for future non-Ruby clients — when pitch 02 (non-Ruby clients) ships, each client can send a hint via `context.app_root` and the highlighting can use that. Don't build configurable highlighting patterns for a problem that doesn't exist yet.

### Error message truncation

Error messages are truncated to 500 chars at the client. The detail page should render the full 500 chars without further truncation. If the message is long, let it wrap — don't add a "show more" toggle for 500 chars of text.

### `introduced_deploy_sha` backfill

Existing error metrics rows won't have the new column populated. For errors whose raw events are still within retention, a one-time backfill query can populate the field. For errors whose raw events have been pruned, the field stays null — "introduced deploy unknown." Don't build an automatic backfill migration — a manual SQL statement in the runbook is sufficient for this one-time transition.

---

## No-gos

- **Error status workflow (new/acknowledged/resolved).** It's a state machine with persistence requirements. Out of scope — the current "new badge" based on first_seen is sufficient for v1.
- **Multi-sample event view.** One sample event per error detail. Not a list of occurrences with individual context.
- **Error grouping changes.** The SHA1 fingerprint algorithm is the algorithm. Don't change grouping in this pitch.
- **Affected users count.** Would require counting distinct `actor_id` per fingerprint. Interesting but not essential — the occurrence count is sufficient.
- **Source code preview.** Showing the actual source line from the stack trace requires repo access. That's a Parachute integration feature, not a Beacon feature.
- **Configurable frame highlighting.** Hardcoded heuristic for Ruby. Revisit when non-Ruby clients exist.

---

## Acceptance Criteria

1. **An agent can investigate an error end-to-end.** Call `beacon.error_detail` with a Maket production fingerprint — get back the stack trace, error message, request path, deploy SHA, and hourly occurrence timeline in one response. Enough to decide "fix it" or "leave it" without switching to the dashboard.
2. **The dashboard error detail page answers "what happened?"** Request context block (path, method, deploy SHA, environment) is visible above the stack trace. App frames are visually distinct from framework frames. The error message is shown in full.
3. **Deploy correlation survives retention.** The `introduced_deploy_sha` is persisted on the metrics row. An error first seen 30 days ago still shows which deploy introduced it, even though the raw events have been pruned.
4. **Graceful degradation.** When the sample raw event has been pruned (error older than 14 days), the detail page and MCP tool still return the rollup data (name, occurrences, timeline, introduced deploy SHA) and clearly indicate that the sample event is no longer available.

---

## Scope

Cards 1-3 are P0. Card 4 is P1.

- [x] **`GetErrorDetail` read handler** — new method on `internal/reads` that takes a fingerprint and returns: error summary (name, first_seen, last_seen, occurrences, `introduced_deploy_sha`), most recent sample event (message, stack_trace, first_app_frame, context, properties), and hourly occurrence timeline. Degrades gracefully when sample event is pruned (returns rollup data only, `sample_event: null`). *Done when:* a Go test seeds error events and metrics, calls `GetErrorDetail`, and asserts all fields are populated. A second test with no raw events (pruned) asserts graceful degradation.

- [x] **MCP `beacon.error_detail` tool** — new MCP tool backed by `GetErrorDetail`. Takes `fingerprint` (required). Returns the full investigation payload (see Solution section). Register in `mcpserver.registerTools()`. *Done when:* MCP test covers: happy path (fingerprint found, sample event present), fingerprint not found (MCP error), and pruned sample (rollup-only response). The tool appears in `tools/list`.

- [x] **Dashboard error detail enrichment** — extend `/errors/:fingerprint` handler and `error_detail.html` template to render: request context block (path, method, deploy SHA, environment, request ID), error message, first app frame, trend indicator (increasing/stable/decreasing), app vs framework frame highlighting in the stack trace (hardcoded Ruby heuristic). "Sample event pruned" notice when raw event is unavailable. *Done when:* the error detail page shows request context and highlighted stack trace for a seeded error event. Manual browser check at 375px/768px/1440px confirms readability.

- [x] **Persist `introduced_deploy_sha`** — add nullable `introduced_deploy_sha VARCHAR(64)` column to `beacon_metrics`. The rollup worker sets it once when creating the first metric row for a new error fingerprint, copying from the event's `context.deploy_sha`. Display on both dashboard ("Introduced in deploy: `<sha>`") and MCP response. *Done when:* a test seeds two error events with different deploy SHAs on different fingerprints and asserts each fingerprint's `introduced_deploy_sha` matches its first event's SHA. A second fingerprint's value is not overwritten by later events.
