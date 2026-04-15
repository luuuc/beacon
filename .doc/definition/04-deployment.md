# Beacon — Deployment

*Definition doc — how Beacon runs in the wild.*

---

## Quickstart

For local development or trying Beacon for the first time:

```bash
curl -fsSL https://raw.githubusercontent.com/luuuc/beacon/main/install.sh | sh
beacon init --database postgres --ruby
docker compose up -d
```

`beacon init` generates a `docker-compose.yml`, `.mcp.json`, and optionally a Rails initializer — starter files you can edit. See `beacon init -h` for all flags.

For production deployments, read on.

---

## The Four Shapes

Beacon ships as one binary that runs in any of these shapes. The binary doesn't know or care which.

| Shape | When to use it | What you write |
|---|---|---|
| **Kamal accessory** | Rails apps deployed with Kamal (the canonical case) | A few lines in `config/deploy.yml` |
| **Docker container** | Any stack with Docker, no Kamal | A `docker run` or `docker-compose.yml` entry |
| **systemd unit** | Bare metal, no containers | A unit file pointing at the binary |
| **`beacon serve`** | Local development, smoke tests | Nothing — just run it |

The Kamal path is first-class because Parachute uses Kamal and Beacon's first audience overlaps heavily with that crowd. The Docker path is the universal fallback. systemd and bare-binary exist for people who want them.

---

## Kamal Accessory (Recommended)

In your `config/deploy.yml`:

```yaml
accessories:
  beacon:
    image: ghcr.io/luuuc/beacon:latest
    host: <your-app-host>
    # Loopback-only host port binding. The app container reaches Beacon
    # via the Kamal Docker network using the container hostname
    # `<service>-beacon` (e.g. `maket-beacon`), NOT via this host port.
    # The host port exists only for SSH-tunnelled debugging from your
    # laptop; delete the line entirely if you don't need that.
    port: "127.0.0.1:4680:4680"
    env:
      clear:
        # Bind 0.0.0.0 INSIDE the container so the app container can
        # reach Beacon across the Kamal Docker network. The loopback
        # guard is enforced at the host boundary by not publishing the
        # port publicly (see `port:` above), not by the bind address.
        BEACON_BIND: "0.0.0.0"
        BEACON_DATABASE_URL: postgres://beacon:5432/beacon_production
        BEACON_DATABASE_SCHEMA: beacon
      secret:
        - BEACON_AUTH_TOKEN        # REQUIRED — the server refuses to
                                   # start on a non-loopback bind with
                                   # no token set.
        - BEACON_DATABASE_PASSWORD
    files:
      - config/beacon.yml:/etc/beacon.yml
    options:
      network: "private"
```

Then:

```bash
bin/kamal accessory boot beacon
```

That's the entire deployment. Beacon comes up, runs migrations against the configured database, and starts serving the API, MCP, and dashboard on port `4680`.

**The Kamal Docker DNS name for the Beacon accessory is `<service>-beacon`**, where `<service>` is the `service:` value at the top of your `config/deploy.yml`. For an app whose `service: myapp`, that's `myapp-beacon`; for Maket it's `maket-beacon`. The Rails client points at `http://<service>-beacon:4680` — do not hardcode a specific name that only exists in someone else's app.

```ruby
# config/initializers/beacon.rb — generic form
Beacon.configure do |c|
  c.endpoint    = "http://#{ENV.fetch('KAMAL_SERVICE', 'myapp')}-beacon:4680"
  c.auth_token  = ENV["BEACON_AUTH_TOKEN"]
  c.environment = Rails.env
end
```

### Same-host accessory (Maket pattern, single-box staging/prod)

Maket runs staging and production each on a dedicated Hetzner AX42-U box. Beacon ships as an accessory on the same box, reusing the existing Postgres accessory (`maket-db`) via a separate schema. The pattern:

```yaml
# config/deploy.staging.yml (and deploy.production.yml mirrors it)
accessories:
  beacon:
    image: ghcr.io/luuuc/beacon:latest
    host: 65.108.239.247   # same host as web/job/db
    port: "127.0.0.1:4680:4680"   # loopback-only publish; optional
    env:
      clear:
        BEACON_BIND: "0.0.0.0"
        BEACON_DATABASE_URL: postgres://maket:5432/maket_staging
        BEACON_DATABASE_SCHEMA: beacon
        BEACON_DATABASE_MAX_CONNS: "8"   # pgx pool cap
        BEACON_RETENTION_EVENTS_DAYS: "14"
      secret:
        - BEACON_AUTH_TOKEN
        - POSTGRES_PASSWORD:BEACON_DATABASE_PASSWORD  # reuse maket's PG password
```

Rails-side wiring (one initializer):

```ruby
# config/initializers/beacon.rb
Beacon.configure do |c|
  c.endpoint    = "http://maket-beacon:4680"   # Kamal Docker DNS name
  c.auth_token  = ENV["BEACON_AUTH_TOKEN"]
  c.environment = Rails.env
end
```

Add `BEACON_AUTH_TOKEN` to `.kamal/secrets.staging` and `.kamal/secrets.production` — two distinct tokens, one per environment. The staging token must never equal the production token; rotating one must not require touching the other.

**Traffic is 100% internal.** The Rails app container and the Beacon container share the Kamal-managed Docker bridge network. Nothing leaves `lo` on the host. No TLS is needed on this hop — `kamal-proxy` terminates TLS for the public site at the edge; the Rails → Beacon hop stays on the bridge.

### External access (HTTPS)

Everything — the API, MCP, and dashboard — runs on a single port (4680). To make Beacon reachable from browsers and AI code tools running on a developer's laptop (Claude Code, Claude Desktop, Cursor), expose it via kamal-proxy on an HTTPS subdomain.

This follows the same pattern Maket uses for Umami and Metabase accessories: a `proxy:` block in the accessory config, a Cloudflare Origin Cert for TLS, and a DNS record pointing to the host.

```yaml
accessories:
  beacon:
    image: ghcr.io/luuuc/beacon:latest
    host: 65.108.239.247
    port: "127.0.0.1:4680:4680"
    proxy:
      host: beacon.maket.store
      healthcheck:
        path: /api/healthz
      ssl:
        certificate_pem: SSL_CERTIFICATE_PEM
        private_key_pem: SSL_PRIVATE_KEY_PEM
      app_port: 4680
    env:
      clear:
        BEACON_BIND: "0.0.0.0"
        BEACON_DATABASE_URL: postgres://maket:5432/maket_staging
        BEACON_DATABASE_SCHEMA: beacon
      secret:
        - BEACON_AUTH_TOKEN
        - POSTGRES_PASSWORD:BEACON_DATABASE_PASSWORD
```

Key points:

- **One port, one proxy block.** The app reaches Beacon via Docker DNS (`http://maket-beacon:4680`); humans reach the dashboard via `https://beacon.maket.store/`; AI agents reach MCP via `https://beacon.maket.store/mcp/rpc`.
- **Health check path is `/api/healthz`**. Configure `healthcheck.path` explicitly — kamal-proxy defaults to `GET /up`.
- **SSL uses a Cloudflare Origin Cert** (or any cert your CDN/reverse proxy accepts). The cert PEM and private key go into `.kamal/secrets` alongside other secret env vars.
- **DNS**: add an A record for `beacon.maket.store` (or your chosen subdomain) pointing to the host IP. If using Cloudflare, set the proxy mode to "Full (Strict)" so Cloudflare validates the Origin Cert.

Once deployed, the MCP endpoint is reachable at `https://beacon.maket.store/mcp/rpc`. The stdio proxy on the developer's machine connects to it:

```bash
BEACON_AUTH_TOKEN=secret beacon mcp proxy https://beacon.maket.store/mcp/rpc
```

Or via `.mcp.json` — see `08-ai-workflow.md` for the full config examples.

**Local development and devcontainers need none of this.** Port 4680 is forwarded from the beacon compose service to the host. The proxy hits `http://localhost:4680/mcp/rpc` directly — no TLS, no certs, no kamal-proxy, no DNS.

### Reusing the app's database

If you want Beacon to share your app's PostgreSQL accessory rather than running its own database:

```yaml
accessories:
  beacon:
    image: ghcr.io/luuuc/beacon:latest
    host: <your-app-host>
    env:
      clear:
        BEACON_DATABASE_URL: postgres://postgres:5432/myapp_production
        BEACON_SCHEMA: beacon  # creates a separate schema in the same DB
      secret:
        - BEACON_DATABASE_PASSWORD
```

Beacon creates its own schema and stays out of your app's tables. You back up one database, you migrate one database, you watch one database. This is the lowest-friction setup for solo founders.

---

## Docker (Any Stack)

```bash
docker run -d \
  --name beacon \
  -p 127.0.0.1:4680:4680 \
  -v $(pwd)/beacon.yml:/etc/beacon.yml \
  -e BEACON_DATABASE_URL=postgres://user:pass@host:5432/beacon \
  ghcr.io/luuuc/beacon:latest
```

Or with `docker-compose.yml`:

```yaml
services:
  beacon:
    image: ghcr.io/luuuc/beacon:latest
    ports:
      - "127.0.0.1:4680:4680"
    volumes:
      - ./beacon.yml:/etc/beacon.yml
    environment:
      BEACON_DATABASE_URL: postgres://user:pass@db:5432/beacon
    restart: unless-stopped
```

Note: Beacon binds to `127.0.0.1` by default. Any non-loopback bind requires `BEACON_AUTH_TOKEN` to be set — the server refuses to start otherwise (see `internal/config/config.go`). The token is a single shared bearer credential: treat it like a database password. The intended deployment is one Beacon per app reached from the app's own containers on a private network; putting Beacon on the public internet is supported (token + TLS terminator in front) but is not the primary shape.

---

## systemd

```ini
# /etc/systemd/system/beacon.service
[Unit]
Description=Beacon observability accessory
After=network.target postgresql.service

[Service]
Type=simple
User=beacon
Group=beacon
ExecStart=/usr/local/bin/beacon serve --config /etc/beacon.yml
Restart=on-failure
RestartSec=5
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

```bash
sudo systemctl enable beacon
sudo systemctl start beacon
journalctl -u beacon -f
```

---

## `beacon.yml`

The config file is small on purpose:

```yaml
# /etc/beacon.yml
server:
  http_port: 4680
  bind: 127.0.0.1

database:
  # Option 1: explicit URL
  url: postgres://beacon:password@localhost:5432/beacon_production

  # Option 2: detect from environment (DATABASE_URL or BEACON_DATABASE_URL)
  # auto: true

  # Option 3: SQLite (zero-config)
  # adapter: sqlite3
  # path: /var/lib/beacon/beacon.db

  schema: beacon  # PostgreSQL only — uses a separate schema in the same DB

retention:
  events_days: 14
  rollups_hour_days: 90
  rollups_day: indefinite

rollup:
  tick_seconds: 60
  prune_at: "03:00"
  timezone: "UTC"

baseline:
  windows: ["24h", "7d", "30d"]

dimensions:
  # Per-metric dimension declarations. Optional.
  signup.completed:
    - plan
  checkout.succeeded:
    - plan
    - country

# Optional: external alerting hooks (Beacon does not own alerting; this is a webhook escape hatch)
# webhooks:
#   on_anomaly: https://your-app.example.com/beacon/anomaly
```

Anything not specified takes a sensible default. The minimum viable config is just `database:`.

---

## Database Setup

Beacon doesn't ship a `bin/beacon db:create`. The database has to exist before Beacon starts; Beacon will create its tables (or schema) inside it.

### Standalone Postgres

```sql
CREATE DATABASE beacon_production;
CREATE USER beacon WITH ENCRYPTED PASSWORD '...';
GRANT ALL PRIVILEGES ON DATABASE beacon_production TO beacon;
```

### Sharing the app's Postgres

```sql
CREATE SCHEMA beacon;
GRANT ALL ON SCHEMA beacon TO myapp_user;  -- or a dedicated beacon_user
```

Then point Beacon at the same `DATABASE_URL` as the app, with `schema: beacon` in `beacon.yml`. Migrations run inside the `beacon` schema. The app's tables are untouched.

### SQLite

```bash
mkdir -p /var/lib/beacon
chown beacon:beacon /var/lib/beacon
```

Beacon will create the file on first run. SQLite is appropriate for: local development, small projects (a few hundred events per minute), and embedded use cases. Above that, use Postgres or MySQL.

---

## Backups

Beacon has no backup story of its own because it doesn't need one. Beacon's data lives in your existing database. **You back up your database; you back up Beacon.**

If you're sharing the app's database (the recommended setup), Beacon's data is included in whatever backup you already run. If you've put Beacon on a dedicated database, treat it like any other Postgres backup target.

The data Beacon stores is mostly disposable in a disaster. Raw events are short-lived. Rollups can be partially recomputed from raw events if recent. The only thing that genuinely matters is **deployment baselines** — losing those means losing the "before" snapshots Patrol uses for outcome verification. Back those up.

---

## Health Checks

Beacon exposes two endpoints for supervisors:

- `GET /api/healthz` — process is alive (returns 200 immediately)
- `GET /api/readyz` — database is reachable, migrations are applied, rollup worker has ticked in the last 5 minutes (returns 200 if all true)

Kamal, Docker, and systemd all consume these. If `/api/readyz` starts failing, the supervisor restarts the binary.

---

## Upgrades

Beacon migrations are forward-compatible within a major version. To upgrade:

1. Pull the new image: `docker pull ghcr.io/luuuc/beacon:latest`
2. Restart the accessory: `bin/kamal accessory restart beacon`
3. Migrations run on boot, idempotently

There is no `bin/beacon migrate` command. Migrations are part of the boot sequence. If a migration fails, the binary refuses to start, the supervisor surfaces the error, and you investigate.

Major version bumps will be rare and will come with a manual upgrade note. For v1, assume forward-compatible auto-migration.

---

## Resource Footprint

The honest numbers (target, not measured yet — v1 shipped):

- **Memory:** ~50 MB resident
- **CPU:** negligible at idle, ~10% of one core under sustained 100 events/sec ingestion
- **Disk:** depends entirely on retention settings and traffic. A small Rails app on the recommended 14-day raw / 90-day hourly retention sits around 200–500 MB.

Beacon is meant to be cheap enough that running it is not a decision you have to budget for. If it's costing you real money to run Beacon, file a bug.
