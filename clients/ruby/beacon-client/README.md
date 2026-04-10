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

In a Rack/Rails app, mount the middleware:

```ruby
# config/application.rb
config.middleware.use Beacon::Middleware, sink: Beacon.client
```

For background jobs and mailers (opt-in):

```ruby
require "beacon/integrations/active_job"
require "beacon/integrations/action_mailer"
Beacon::Integrations::ActiveJob.install
Beacon::Integrations::ActionMailer.install
```

For Puma in clustered mode:

```ruby
# config/puma.rb
on_worker_boot { Beacon.client.after_fork }
```

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

See `doc/definition/05-clients.md` and `doc/definition/07-writing-a-client.md`
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
