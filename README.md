# Beacon

**The small observability accessory for self-hosted apps.**

Beacon answers three questions about anything you ship:

1. Did the feature work?
2. Is anything slowing down or shifting in volume?
3. Is anything breaking?

One Go binary, one database you already run (PostgreSQL, MySQL, or SQLite), three pillars — outcomes, performance, errors. Readable by both humans and AI agents. Ships as a Kamal accessory, a Docker container, a systemd unit, or a bare binary.

Not Datadog. Not Sentry. Not PostHog. The lightweight substrate a solo founder or small team actually needs.

> **Status:** alpha, not yet released. Currently being proven against a real Rails 8 / Kamal / Hetzner app before external users.

---

## What Beacon gives you

- **Outcomes** — `Beacon.track("signup.completed", user: current_user, plan: "pro")`. Count anything, roll it up, compare against a trailing baseline, know whether the last deploy moved the number.
- **Performance** — automatic per-endpoint P50/P95/P99 with deploy annotations, captured by a 50 µs-budgeted Rack middleware.
- **Errors** — automatic exception capture with first-app-frame fingerprinting, stack-trace throttling, and new-vs-seen grouping.

All three pillars live in the same database, read by the same HTTP API, queryable by the same MCP tools. One backend, one auth token, one thing to operate.

## Architecture in one diagram

```
┌────────────────┐    HTTP POST /events    ┌──────────────────┐
│  Your app      │ ──────────────────────▶ │  beacon (Go)     │
│  beacon-client │                         │  ingest + rollup │
│  (Ruby gem)    │                         │  + MCP server    │
└────────────────┘                         └──────┬───────────┘
                                                  │
                                                  ▼
                                         ┌──────────────────┐
                                         │  PostgreSQL /    │
                                         │  MySQL / SQLite  │
                                         │  (you run it)    │
                                         └──────────────────┘
```

Beacon does not own a database. It owns a schema or a file inside one you're already backing up. Losing Beacon's data in a disaster costs you a couple of weeks of observability history, not a business.

## Hot-path overhead

On Ruby 3.4.4 / arm64:

| Surface                         | Added P95 latency |
|---------------------------------|-------------------|
| `Beacon::Middleware` (NullSink) | **0 ns**¹         |
| `Beacon::Middleware` + real queue + flusher | **1 µs**         |

Both are 50× under the committed 50 µs ceiling. The Rack middleware allocates one Hash and one cached path key per request, pushes to a bounded background queue, and returns. Nothing synchronous on the host's hot path.

¹ At the clock-resolution floor — `Process.clock_gettime` can't measure it.

## Deploy shapes

| Shape | When | What you write |
|---|---|---|
| **Kamal accessory** | Rails on Kamal (first-class) | A few lines in `deploy.yml` |
| **Docker container** | Any stack with Docker | A `docker run` or compose entry |
| **systemd unit** | Bare metal | A unit file |
| **`beacon serve`** | Local dev | Nothing — just run it |

See the deploy recipes linked in the operator docs shipped with the binary.

## AI agent access (MCP)

AI code tools query Beacon via MCP over a stdio proxy baked into the binary. Install the binary on your dev machine, then point your tool's MCP config at it.

```bash
# Install
go install github.com/luuuc/beacon/cmd/beacon@latest
```

Add a `.mcp.json` to your project root:

```json
{
  "mcpServers": {
    "beacon": {
      "command": "beacon",
      "args": ["mcp", "proxy", "http://localhost:4681/rpc"],
      "env": { "BEACON_AUTH_TOKEN": "devtoken" }
    }
  }
}
```

For staging/production, replace the URL with an HTTPS endpoint (e.g. `https://beacon-mcp.example.com/rpc`) and set the token from an environment variable.

Six read-only tools appear in Claude Code, Claude Desktop, or Cursor: `beacon.errors`, `beacon.perf_drift`, `beacon.metric`, `beacon.compare`, `beacon.outcome_check`, and `beacon.deploy_baseline`.

## Clients

- **Ruby** (`clients/ruby/beacon-client`) — the reference client. Rack middleware + `ActiveSupport::Notifications` subscribers. Zero runtime gem dependencies; pure stdlib transport. Works in Rails, Sinatra, and raw Rack.
- Other languages — planned, not yet shipped.

## License

O'Saasy. See [`LICENSE`](./LICENSE) or https://osaasy.dev.

TL;DR: MIT-style for all non-commercial-SaaS use. You can run, modify, embed, and even resell Beacon as part of a larger product — you just cannot turn around and offer a hosted Beacon-as-a-Service to third parties in competition with the original author. If that's a problem for you, reach out.

## Status and roadmap

Alpha. Not released. Currently closing the last cards of the bootstrap pitch — packaging, acceptance drills on a reference Rails app, then a dashboard engine. External users welcome once the first acceptance run is green.
