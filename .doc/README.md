# Beacon

**The small observability accessory for self-hosted apps.**

Beacon answers three questions about anything you ship:

1. **Did the feature work?**
2. **Is anything slowing down or shifting in volume?**
3. **Is anything breaking?**

One binary, one database, three pillars. No SaaS account, no agent, no DSL to learn. Beacon plugs into the database you already run (PostgreSQL, MySQL, or SQLite) and starts working.

It's not Datadog. It's not Sentry. It's not PostHog. It's the lightweight thing in between — the observability substrate a small team or a solo founder actually needs, designed so both humans and AI agents can read it.

---

## Install

```bash
# One-line install (macOS / Linux)
curl -fsSL https://raw.githubusercontent.com/luuuc/beacon/main/install.sh | sh
```

Then generate starter files:

```bash
beacon init --database postgres --ruby
# Creates: docker-compose.yml, .mcp.json, config/initializers/beacon.rb

docker compose up -d && bundle add beacon-client && rails s
```

Or run with Docker directly:

```bash
docker run -p 4680:4680 -e BEACON_DATABASE_URL=postgres://user:pass@host:5432/beacon ghcr.io/luuuc/beacon:latest
```

For Kamal deployments, see [definition/04-deployment.md](definition/04-deployment.md).

That's it. Errors and request performance are captured automatically. Outcomes are tracked when you call `Beacon.track`.

---

## How It Works

### Did the feature work?

```ruby
Beacon.track("signup.completed", user_id: user.id, plan: "pro")
```

Beacon stores named events, rolls them up, and remembers what they looked like before you shipped. Ask "is the new signup flow working" and Beacon compares the current window to the baseline it captured the day you deployed.

### Is anything slowing down or shifting in volume?

Nothing. Beacon's Rack middleware captures every HTTP request automatically — path, status, duration, throughput. Same for background jobs. Per-endpoint P50/P95/P99 rolled up hourly and daily, with a trailing baseline so "current vs normal" is always one query away.

### Is anything breaking?

Nothing. The same middleware captures exceptions, fingerprints them (exception class + first app frame), and groups occurrences. New fingerprints stand out from the baseline immediately. Background job failures and mailer delivery errors flow into the same store.

---

## Three Surfaces

Beacon exposes its data three ways:

- **HTTP API** — `GET /api/metrics/signup.completed?window=7d` for any stack, any tool
- **MCP server** — so AI agents can ask "is `/checkout` slowing down?" and get a structured answer
- **`beacon-rails` dashboard** — a small mountable engine for humans who want to look

The dashboard exists. It is not the point. Beacon is built to be read by code first.

---

## What Beacon Is Not

- **Not Datadog.** No log aggregation, no distributed tracing, no thirty-feature platform.
- **Not Sentry.** Error handling good enough for the 90% case. If you need Sentry's depth, use Sentry — Beacon will read it as a signal.
- **Not PostHog.** No funnels, no session replay, no feature flags.
- **Not a dashboard tool.** The dashboard is secondary. Code is the primary consumer.

The negative space matters. Beacon does the three pillars cleanly and stops.

---

## Status

Alpha. Not yet released.

Beacon is being built standalone first, then proven against [Maket](https://maket.store) — a production Rails 8 app — before [Parachute](https://para.chute.one) depends on it. Parachute's Patrol skill will eventually read errors, performance, and outcome data from Beacon's MCP server to decide whether what just shipped is healthy. Any other AI workflow can do the same.

See `decisions/0002-maket-first-integration.md` for the rollout order: Maket staging → Maket production → external users → Parachute.

---

## Docs

- [Purpose](definition/01-purpose.md) — what Beacon is and why it exists
- [Architecture](definition/02-architecture.md) — process model, components, why Go
- [Data Model](definition/03-data-model.md) — schema, retention, query patterns
- [Deployment](definition/04-deployment.md) — Kamal, Docker, systemd, bare binary
- [Ruby Client and Rails Dashboard](definition/05-clients.md) — `beacon-client` gem, impact on the host app, `beacon-rails` dashboard
- [HTTP API](definition/06-http-api.md) — normative contract: auth, schemas, fingerprint algorithm, path normalization, MCP
- [Writing a Client](definition/07-writing-a-client.md) — invariants any language-specific client must preserve
- [AI Code Workflow](definition/08-ai-workflow.md) — how Claude Code, Cursor, Codex and other agents use Beacon before, during, and after shipping code
- [Runbook](definition/09-runbook.md) — what to do when Beacon misbehaves in production

## Pitches

- [00 — Bootstrap (v1)](pitches/00-bootstrap.md) — the three pillars, one accessory, one Rails client
  - [00b — Ruby client hardening](pitches/00b-ruby-client-hardening.md) — production-ready gem
  - [00c — MCP client access](pitches/00c-mcp-client-access.md) — stdio proxy + external HTTPS
  - [00d — Binary-hosted dashboard](pitches/00d-dashboard.md) — Go templates + htmx
  - [00e — Unified port](pitches/00e-unified-port.md) — merge MCP onto port 4680
- [01 — Ambient anomalies (v1.1)](pitches/01-ambient-anomalies.md) — passive ingestion + drift detection
  - [01a — Noise filtering + anomaly curation](pitches/01a-noise-and-anomaly-curation.md) — bot path exclusion, 3σ floor, anomaly dismiss
  - [01b — Errors depth](pitches/01b-errors-depth.md) — error detail in MCP + dashboard (stack trace, request context, deploy correlation)
  - [01c — Outcomes depth](pitches/01c-outcomes-depth.md) — Maket instrumentation, deploy annotations, baseline bands
  - [01d — Performance depth](pitches/01d-performance-depth.md) — volume context, latency distribution
  - [01e — Distribution](pitches/01e-distribution.md) — GoReleaser, install script, `beacon init`
- [02 — Non-Ruby clients (v1.2)](pitches/02-non-ruby-clients.md) — Node, Python, Go thin wrappers
- [03 — Parachute integration](pitches/03-parachute-integration.md) — when Beacon lands inside Parachute as Patrol's substrate

## Decisions

- [0001 — Name and scope](decisions/0001-name-and-scope.md) — why Beacon, why three pillars
- [0002 — Maket is Beacon's first integration target](decisions/0002-maket-first-integration.md) — rollout order and co-location with Maket on Hetzner
