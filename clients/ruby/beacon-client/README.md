# beacon-client

The Ruby client for [Beacon](https://github.com/luuuc/beacon) — the small
observability accessory for self-hosted apps.

One initializer wires up three pillars:

- **Performance** — every Rack request is auto-instrumented
- **Errors** — every unhandled exception is fingerprinted and shipped
- **Outcomes** — `Beacon.track("signup.completed", user: current_user)`

## Install

```ruby
gem "beacon-client"
```

> **Do not add `require: "beacon/testing"` in your Gemfile.** The `beacon/testing` file contains test helpers (`NullSink`, `FakeTransport`, `Beacon::Testing.reset_config!`) that should only be loaded from `spec/test_helper.rb` — loading them into production Rails boot is a footgun that leaks test-only classes into your host namespace. `beacon-client` itself is safe to auto-require; only `beacon/testing` is not.

## Configure

```ruby
# config/initializers/beacon.rb
Beacon.configure do |c|
  c.endpoint    = "http://beacon:4680"
  c.environment = Rails.env
  c.deploy_sha  = ENV["GIT_SHA"]                            # optional
  c.auth_token  = Rails.application.credentials.beacon_token # optional
end
```

In a Rails app, that's **all** you write. The gem ships a Railtie that:

- Inserts `Beacon::Middleware` into the stack, right after `ActionDispatch::DebugExceptions` (so host errors flow through Beacon before Rails renders them).
- Auto-installs the ActiveJob and ActionMailer integrations — no `require "beacon/integrations/..."` needed.
- Installs a `Process._fork` hook that runs `Beacon.client.after_fork` in every fork child, so clustered Puma / Unicorn / Passenger workers get their own flusher thread automatically. **No manual `on_worker_boot` needed.**

In a plain Rack app (no Rails), mount the middleware manually:

```ruby
# config.ru
require "beacon"
require "beacon/middleware"
use Beacon::Middleware
```

### Kill switch

To silence Beacon entirely without removing the gem:

```ruby
# config/initializers/beacon.rb
Beacon.configure { |c| c.enabled = false }
```

Or at the operating-system level:

```bash
BEACON_DISABLED=1 bin/rails server
```

A disabled Beacon is a pure passthrough: the middleware adds one
boolean check per request, nothing is captured, no flusher thread
is started, no network connection is opened.

**`BEACON_DISABLED` is read once at process start.** Setting it after
the Ruby process has already booted has no effect — you must restart
the worker. Accepted truthy values: `1`, `true`, `yes`, `on`
(case-insensitive). Everything else (including `0`, `false`, `no`,
`off`, and the empty string) leaves Beacon enabled.

If `c.endpoint` is nil or unparseable, Beacon prints one boot warning
to stderr and then behaves the same as `c.enabled = false` — no crash,
no spam, no network traffic.

### A note on the fork hook

Because the Railtie prepends `Process._fork`, Beacon's `after_fork` runs in
**every** forked child in the process — not just Puma workers. Short-lived
forks like `rails runner`, `system`, and `Open3` subshells will briefly
initialize Beacon in the child. The reinit is idempotent and the flusher is
bounded, but it's a global behavior worth knowing about when you see
`beacon-flusher` threads show up in unexpected places.

## Usage

```ruby
Beacon.track("signup.completed", user: current_user, plan: "pro")
Beacon.track("checkout.failed",  user: current_user, reason: "card_declined")
Beacon.flush  # synchronous, drains the queue (rake tasks, shutdown)
```

## Hot-path guarantees

- **<50µs added P95** on a reference Rack endpoint (enforced by
  `spec/bench/rack_overhead_bench.rb` in CI — the bench fails the build if
  the middleware regresses)
- **Bounded queue** with oldest-drop semantics (default 10,000 events)
- **Rescue-all** — Beacon never raises into the host application
- **Fork-safe** — re-spawns the flusher in clustered Puma/Unicorn workers
- **Idempotency keys** on every retry so safe retries never double-count

See `.doc/definition/05-clients.md` and `.doc/definition/07-writing-a-client.md`
in the Beacon repo for the full contract.

## Development

```bash
gem install minitest rack
rake test    # 32 tests, 102 assertions
rake bench   # Rack overhead bench, fails if added P95 > 50µs
rake         # both
```

The conformance fixtures live at `../../../spec/fixtures.json` (shared with
the Go reference server). Fingerprint and path-normalization tests load
those fixtures directly so client and server can never drift.
