# Pitch 01c — Outcomes depth

*Prove the most differentiated pillar works end-to-end. Follow-up to pitch 00 (outcome tracking via `Beacon.track`, cards 1-3), pitch 00d (outcomes dashboard pages, cards 5-6), and the AI workflow loop described in `definition/08-ai-workflow.md`.*

**Appetite:** small batch (~3-5 days)
**Status:** Shipped — pending PR
**Owner:** Solo founder + AI producers
**Predecessor:** `pitches/00-bootstrap.md` (cards 1-3: outcome events, `beacon.metric` + `beacon.outcome_check` + `beacon.compare` MCP tools), `pitches/00d-dashboard.md` (cards 5-6: outcomes pillar page + detail), `pitches/00b-ruby-client-hardening.md` (Rails Railtie auto-fires `deploy.shipped` on boot)
**Related:** `definition/01-purpose.md` ("Did the feature work?" — the first of Beacon's three questions), `definition/08-ai-workflow.md` (the three-phase before/during/after loop — outcome check is the "after" payoff), `definition/03-data-model.md` (outcome events, deploy baselines), `decisions/0002-maket-first-integration.md` (Maket as proof point)
**Precondition:** Maket outcome instrumentation deployed independently before this pitch starts (five `Beacon.track` calls for `signup.completed`, `listing.created`, `order.placed`, `offer.accepted`, `payout.completed`). Data must be accumulating.

---

## Problem

The outcomes pillar is Beacon's differentiator — the thing that makes it more than "a worse Sentry + a worse Skylight." It answers "did the feature work?" — a question no other lightweight observability tool even tries to answer. Pitch 00 built the infrastructure: `Beacon.track` on the client, outcome rollups on the server, `beacon.outcome_check` and `beacon.compare` MCP tools, and deploy baseline capture. Pitch 00d added outcomes list and detail pages to the dashboard.

The pillar is empty. Maket has been running Beacon in production for over a week and has zero outcome events tracked. The dashboard outcomes page shows nothing. The `beacon.outcome_check` MCP tool has never been exercised against real data. The entire "did the feature work?" loop described in `08-ai-workflow.md` is aspirational.

This is the most important gap in the product. Without outcomes, Beacon's pitch to a new user is "auto-captured errors and perf" — which is what every observability tool already does, just worse. With outcomes, Beacon's pitch is "the only self-hosted tool that closes the loop between shipping code and verifying it worked." That's a reason to exist.

Maket outcome instrumentation will be deployed independently before this pitch starts — five `Beacon.track` calls for the core business moments. This pitch focuses on making the dashboard and MCP surface worthy of the data.

Two problems to solve:

1. **The outcomes dashboard shows counts, not context.** The current view is: metric name, sparkline, daily count, drift percentage. That answers "is the number going up or down?" but not "is this good or bad?" or "what changed?" An outcome that dropped 30% after a deploy should look alarming. An outcome that doubled after a marketing campaign should look like a win. The current dashboard treats both the same — a number with a drift arrow.

2. **The outcome check loop has never been verified.** The MCP tools exist (`beacon.metric`, `beacon.outcome_check`, `beacon.compare`) but have never run against real outcome data. There may be bugs, confusing response formats, or missing context that only shows up with real numbers.

---

## Appetite

**Small batch — 3 to 5 days.** The dashboard and MCP improvements are modest — the data and views already exist, they just need context and polish. The biggest time risk is ensuring deploy events are flowing correctly so deploy annotations have data to render.

---

## Solution

### Dashboard: outcome context

Enhance the outcomes list and detail pages:

**Outcomes list (`/outcomes`):**
- Add deploy markers to sparklines — vertical lines at deploy timestamps so the user can see "the number changed here, and that's when we deployed"
- Add absolute count alongside drift percentage ("142/day, ↑12% vs baseline" not just "↑12%")
- Sort: biggest absolute drift first (not percentage — a 50% drop on 2 events/day is less important than a 10% drop on 1000 events/day)

**Outcome detail (`/outcomes/:name`):**
- Deploy annotations on the time-series chart (vertical lines with deploy SHA labels)
- Baseline ±1σ band visualization — shade the range around the baseline mean so the user can see at a glance whether current values are inside or outside normal. ±1σ is consistent with Beacon's existing drift labeling (>1σ = drift).
- "Last deploy" context block: deploy SHA, time since deploy, verdict (pass/drift/fail based on current vs deploy baseline)

The deploy annotation data comes from `beacon_metrics` rows where `kind = 'baseline'` and `period_kind = 'baseline'` — these are written when `deploy.shipped` events arrive. The Railtie already fires `deploy.shipped` on Rails boot.

### Baseline band rendering

The ±1σ band on the SVG chart requires `ChartSVG` to accept baseline mean and stddev and render a filled semi-transparent `<polygon>` behind the data line. ~20 lines of SVG. The band color inherits from CSS custom properties (works in both light and dark mode). A single band — no nested ±2σ shading, no gradient.

### MCP: verify the outcome check loop

The MCP tools for outcomes already exist (`beacon.metric`, `beacon.outcome_check`, `beacon.compare`). This pitch doesn't add new tools — it **verifies they work end-to-end** against real Maket data and fixes any gaps found.

Specifically:
- Confirm `beacon.outcome_check` returns meaningful verdicts for the five Maket events after a deploy
- Confirm `beacon.compare` works with real deploy baselines
- If any tool returns unhelpful or confusing results with real data, fix the response format

This is test-and-fix, not new development. The MCP surface should already work — the pitch proves it does.

---

## Rabbit holes

### Deploy markers require deploy events

Deploy annotations on charts require `deploy.shipped` events in the database. The Railtie fires this automatically on Rails boot, but only if `deploy_sha` is configured. Verify Maket's `beacon.rb` initializer sets `c.deploy_sha = ENV["KAMAL_VERSION"]` (or `ENV["GIT_SHA"]`). If deploy events aren't flowing, fix the initializer — don't build the annotation feature without data.

### Sorting by absolute drift

Sorting outcomes by absolute drift (count × percentage) instead of percentage alone avoids the "50% drop on 2 events" false alarm. But it also means a high-volume event with a small drift always outranks a low-volume event with a large drift. This is the right default for a triage view — high-volume events matter more. Don't build configurable sorting for v1.

### Baseline band visualization

Shading the ±1σ band on the SVG chart is ~20 lines of Go. Don't over-complicate the rendering — a single semi-transparent polygon behind the data line is enough. If the band is too wide on low-traffic outcomes (stddev larger than the mean), let it clip to zero on the Y axis. Don't add special handling for edge cases.

### Waiting for data

Outcome events accumulate slowly on a marketplace with modest traffic. `order.placed` might be 10-50/day on Maket. A meaningful baseline requires at least 3 days of data, and a meaningful deploy comparison requires data on both sides of a deploy.

Don't block the pitch on data accumulation. Work on dashboard improvements using seeded test data, then verify against real data once available. If Maket traffic is too low for a confident verdict by end-of-pitch, document the state and move on — the infrastructure is in place.

---

## No-gos

- **Funnel visualization.** "Signup → Listing → Order" is a funnel. Beacon doesn't do funnels — that's PostHog territory. Each outcome is independent.
- **Conversion rate calculation.** Beacon tracks absolute counts, not rates. "What percentage of signups become orders?" requires joining outcomes — out of scope.
- **Outcome alerting.** No notifications when an outcome drops. Beacon reports; the user or agent decides.
- **Custom dimensions on the outcomes list.** The dashboard shows total counts. Dimension breakdowns (by plan, by country) are a future enhancement.
- **A/B test support.** No control/treatment groups. Beacon compares against historical baselines, not against a concurrent control.

---

## Acceptance Criteria

1. **Outcomes pillar has real data.** The dashboard outcomes page shows cards for all five Maket business events with sparklines and baseline drift indicators.
2. **Deploy annotations tell the story.** The outcome detail chart shows vertical lines at deploy timestamps. A user can visually connect "the number changed here" to "that's when we deployed."
3. **Baseline band shows normal.** The ±1σ band on the outcome detail chart makes it immediately obvious whether current values are inside or outside the normal range, without reading numbers.
4. **The outcome check loop closes end-to-end.** `beacon.outcome_check` on a real Maket event with a real deploy timestamp returns a meaningful pass/drift/fail verdict.
5. **Deploy events are flowing.** `beacon.deploy_baseline` returns a recent baseline for Maket metrics, timestamped to the last Kamal deploy.

---

## Scope

Cards 1-2 are P0. Card 3 is P1.

- [x] **Data foundation verification** — confirm outcome events are flowing for all five business events (`signup.completed`, `listing.created`, `order.placed`, `offer.accepted`, `payout.completed`). Confirm `deploy.shipped` events are firing on Kamal deploys with the correct deploy SHA. Confirm deploy baselines appear in `beacon_metrics`. Fix Maket's initializer if needed. *Done when:* `beacon.metric` returns data for at least one outcome event, and `beacon.deploy_baseline` returns a recent baseline timestamped to the last deploy. ⚠ **Partial**: deploy events flowing (27 over 4 days), deploy baseline capture working (verified on perf metrics, e.g. `GET /items/:id` baseline captured 2026-04-14T15:03:43Z). Business outcome events not yet instrumented in Maket — all five return empty. Infrastructure is proven; outcome data pending Maket-side `Beacon.track` deployment.

- [x] **Dashboard outcomes enhancement** — outcomes list: add absolute count alongside drift, add deploy marker lines on sparklines, sort by absolute drift. Outcome detail: deploy annotations on time-series chart (vertical lines with SHA labels), baseline ±1σ band shading (semi-transparent polygon behind data line, inherits CSS color variables), "Last deploy" context block (SHA, time since deploy, verdict). *Done when:* the outcomes detail page shows a chart with baseline band and deploy markers for a seeded outcome metric. Manual browser check confirms layout at 375px/768px/1440px in both light and dark mode.

- [x] **End-to-end outcome check verification** — exercise `beacon.outcome_check` and `beacon.compare` against real Maket outcome data after a deploy. Document any response format issues found and fix them. *Done when:* a real `beacon.outcome_check` call on a Maket outcome event with a real deploy timestamp returns a meaningful pass/drift/fail verdict. **Found and fixed:** sub-second timestamp precision bug in `CompareDeployBaseline` (silent "insufficient" verdicts), missing JSON tags on `Comparison` struct. Production verification pending Maket instrumentation + deploy.
