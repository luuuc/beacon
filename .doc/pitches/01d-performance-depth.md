# Pitch 01d — Performance depth

*Give the performance pillar enough context to answer "should I worry about this?" Follow-up to pitch 00 (perf capture + `beacon.perf_drift` MCP tool, cards 7-9), pitch 00d (performance dashboard pages, cards 6-7), and pitch 01a (bot filtering — must ship first to clean the data).*

**Appetite:** small batch (~2 days)
**Status:** Shipped — pending PR
**Owner:** Solo founder + AI producers
**Predecessor:** `pitches/00-bootstrap.md` (cards 7-9: perf events, rollups, `beacon.perf_drift` MCP tool), `pitches/00d-dashboard.md` (cards 6-7: performance pillar page + detail), `pitches/01a-noise-and-anomaly-curation.md` (bot filtering — must ship first)
**Related:** `definition/06-http-api.md` (`GET /api/perf/endpoints`, `beacon.perf_drift` MCP tool), `definition/03-data-model.md` (perf rollups: count, p50, p95, p99 per hour)

---

## Problem

Pitch 00 shipped performance capture: the Ruby client instruments every request and job, the server rolls up P50/P95/P99 per endpoint, and `beacon.perf_drift` ranks endpoints by drift from baseline. Pitch 00d added performance list and detail pages to the dashboard. The pillar tells you what's drifting and by how much. It doesn't tell you whether to care.

After bot filtering (01a) cleans the noise, the performance pages show real endpoints with real drift numbers. But the current presentation still leaves questions unanswered:

1. **No volume context.** An endpoint at P95 207ms (+7.4σ drift) could be handling 2 requests/day or 2000. The response to each is very different. The performance cards and detail page show latency in isolation — there's no request count anywhere on the performance views.

2. **No volume trend on the detail page.** The detail page shows a P95 time-series chart. That's a single line — latency over time. There's no way to see whether a latency spike correlates with a traffic spike. "P95 doubled" means something very different when volume was constant vs when volume also tripled.

3. **MCP tool lacks volume context.** `beacon.perf_drift` returns `current_p95`, `baseline_p95`, `drift_sigmas` — no request count. An agent can't distinguish "drifting endpoint with 3 requests" from "drifting endpoint with 3000 requests" and therefore can't triage.

These are small gaps. The performance pillar is functional — it just needs enough additional context to be actionable without clicking through to the time-series chart every time.

---

## Appetite

**Small batch — ~2 days.** Request volume is already in the data (hourly rollups include `count`). Adding it to the cards and MCP response is a field addition. The volume chart is a second call to the existing `ChartSVG` function. No new data layer, no new queries beyond summing existing rollup counts.

**This pitch must ship after 01a (noise filtering).** Building performance depth on a noisy dataset wastes effort — you'd design and test against data that includes 150 bot endpoints. After 01a, the performance views show ~30-50 real endpoints and the depth work lands on clean ground.

---

## Solution

### Volume on performance cards and MCP

**Dashboard performance list (`/performance`):**
- Add request count to each card: "1,324 req/day" alongside the P95 and drift
- The count comes from summing hourly rollup `count` values across the window

**Performance detail (`/performance/:name`):**
- Show total requests in the window alongside the P95 stats

**MCP `beacon.perf_drift` response:**
- Add `request_count` field to each endpoint in the response (sum of hourly counts in the window)
- This data already exists — the hourly rollups include `count`. Sum across the window.

### Volume chart on detail page

A second chart below the existing latency time-series chart on the performance detail page. Same X-axis time range, same hourly resolution. Shows request count per hour as a line chart (using the existing `ChartSVG` function with the `count` field from hourly rollups).

Two stacked charts, not an overlay. Each chart has one Y-axis and one concern. The shared time axis makes latency-volume correlation visually obvious without the complexity of dual-axis rendering.

---

## Rabbit holes

### Volume-weighted sorting

Sorting by "volume × drift" instead of pure drift changes which endpoints surface first. This is probably right for triage, but it's a judgment call. Don't change the sort order in this pitch — the current drift-descending sort is understood. Add volume to the card and let the user form their own triage priority. If everyone keeps clicking the high-volume endpoints first, add volume-weighted sorting in a follow-up.

### Volume chart Y-axis scale

Request counts can vary wildly between endpoints (health checks at 2,800/day vs admin pages at 20/day). The Y-axis auto-scales per endpoint, which is the right behavior — each chart shows its own traffic pattern. Don't normalize across endpoints or add a fixed scale.

---

## No-gos

- **Request tracing.** Beacon does not store individual request details (path, params, user) for perf events. That's Datadog territory.
- **Slow request log.** No "show me the 10 slowest requests" feature. Beacon tracks aggregates, not individual requests.
- **Latency histogram.** No request distribution buckets. The P50/P95/P99 numbers already communicate the distribution shape. A histogram would require querying raw events (heavy) or a new rollup schema (over-engineering).
- **P50/P95/P99 dot visualization.** The percentile numbers are already shown as text stats. A visual indicator adds polish but not information. Skip for v1.
- **Latency alerting thresholds.** No "alert when P95 > 500ms." Beacon reports drift from baseline, not against an absolute threshold.
- **Volume-weighted sorting.** Current drift-descending sort stays. Volume is shown on the card for the user to factor in.

---

## Acceptance Criteria

1. **Every perf card shows volume.** The performance list page and `beacon.perf_drift` MCP response both include request count. A user or agent can distinguish "drifting endpoint with 3 requests" from "drifting endpoint with 3,000 requests" without clicking through.
2. **The detail page shows both latency and volume.** Two stacked charts — latency time-series on top, volume time-series below — sharing the same X-axis time range. The user can visually correlate latency spikes with traffic changes.
3. **This pitch ships after 01a.** The performance views show only real endpoints (bot noise filtered at ingest), so the depth work lands on clean data.

---

## Scope

Both cards are P0.

- [x] **Volume on perf cards + MCP** — add request count (sum of hourly counts in window) to performance list cards, performance detail stats, and `beacon.perf_drift` MCP response. Dashboard: "1,324 req/day" alongside P95 and drift on each card. MCP: `request_count` field on each endpoint. *Done when:* the performance card shows volume, the MCP response includes `request_count`, and a Go test asserts the field is present and correct.

- [x] **Volume chart on detail page** — add a request volume chart below the latency time-series chart on the performance detail page. Uses hourly `count` data from existing rollups. Same X-axis time range as the latency chart. Rendered via existing `ChartSVG` function. *Done when:* the detail page shows both latency and volume charts for a seeded endpoint. Manual browser check confirms layout at 375px and 1440px widths.
