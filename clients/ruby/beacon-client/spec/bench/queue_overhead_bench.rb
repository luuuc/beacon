# Beacon — Rack overhead benchmark (real Queue path).
#
# Card 9. The sibling bench, `rack_overhead_bench.rb`, measures the
# middleware against `Beacon::Testing::NullSink` — a sink that does
# nothing. That's the LOWER bound: the absolute floor of what the
# middleware can achieve if the sink is free. It's useful but it
# doesn't match production, because production uses `Beacon::Client`,
# whose `#push` acquires a Mutex and interacts with a bounded queue.
#
# This bench measures the full production hot path:
#
#   Rack request
#     → Beacon::Middleware
#       → Beacon::Client#push
#         → Beacon::Queue (Mutex + ConditionVariable signal)
#       ← returns nil
#     ← returns to the host
#
# A background Flusher runs against a discarding transport (Results
# that always return 202 without touching the network) so the queue
# drains and the Mutex stays uncontended in the steady state. This
# is what every production box looks like when Beacon is healthy —
# we're measuring the overhead the host actually pays, not a stub.
#
# Same 50µs P95 ceiling as the NullSink bench. If this bench fails,
# the middleware → client → queue path has regressed. Tune the code,
# not the threshold.

$LOAD_PATH.unshift File.expand_path("../../lib", __dir__)

require "minitest/autorun"
require "rack/mock"
require "beacon"
require "beacon/middleware"
require "beacon/testing"

class QueueOverheadBench < Minitest::Test
  ITERATIONS      = 20_000
  WARMUP          = 2_000
  MAX_OVERHEAD_NS = 50_000  # 50 microseconds

  REFERENCE_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.environment     = "bench"
      c.flush_interval  = 0.05
      c.flush_threshold = 100
      c.async           = true
    end
    @transport = Beacon::Testing::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport)
  end

  def teardown
    @client&.shutdown
    Beacon::Testing.reset_config!
  end

  def test_added_p95_under_fifty_microseconds_through_real_queue
    bare         = REFERENCE_APP
    instrumented = Beacon::Middleware.new(REFERENCE_APP, sink: @client, config: Beacon.config)

    request = Rack::MockRequest.env_for("/dashboard", method: "GET")

    bare_p95         = measure_p95(bare, request)
    instrumented_p95 = measure_p95(instrumented, request)

    delta_ns = instrumented_p95 - bare_p95

    warn "[bench queue] bare P95=#{bare_p95}ns instrumented P95=#{instrumented_p95}ns " \
         "delta=#{delta_ns}ns (limit #{MAX_OVERHEAD_NS}ns)"

    assert_operator delta_ns, :<=, MAX_OVERHEAD_NS,
      "Beacon middleware+Client+Queue added #{delta_ns}ns to P95 " \
      "(limit #{MAX_OVERHEAD_NS}ns). Bare P95: #{bare_p95}ns. " \
      "Instrumented P95: #{instrumented_p95}ns. This is the full " \
      "production hot path — the regression is in the Middleware, " \
      "Client, or Queue Mutex path. Check recent changes there."
  end

  private

  def measure_p95(app, request)
    WARMUP.times { app.call(request.dup) }
    samples = Array.new(ITERATIONS)
    ITERATIONS.times do |i|
      env = request.dup
      t0 = Process.clock_gettime(Process::CLOCK_MONOTONIC, :nanosecond)
      app.call(env)
      samples[i] = Process.clock_gettime(Process::CLOCK_MONOTONIC, :nanosecond) - t0
    end
    samples.sort!
    samples[(ITERATIONS * 0.95).to_i]
  end
end
