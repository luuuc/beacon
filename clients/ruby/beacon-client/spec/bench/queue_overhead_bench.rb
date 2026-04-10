# Beacon — Rack overhead benchmark (real Queue path, drain contention).
#
# Card 9. The sibling bench, `rack_overhead_bench.rb`, measures the
# middleware against `Beacon::Testing::NullSink` — a sink that does
# nothing. That's the LOWER bound: the absolute floor of what the
# middleware can achieve if the sink is free. It's useful but it
# doesn't match production, because production uses `Beacon::Client`,
# whose `#push` acquires a Mutex and interacts with a bounded queue
# being drained concurrently by the Flusher's background thread.
#
# This bench measures the full production hot path UNDER REAL
# DRAIN CONTENTION:
#
#   Rack request
#     → Beacon::Middleware
#       → Beacon::Client#push
#         → Beacon::Queue#push  (Mutex + ConditionVariable signal)
#       ← returns nil
#     ← returns to the host
#
#   ... while concurrently ...
#
#   Flusher background thread
#     → wait_and_drain (wakes on threshold or every 1ms)
#       → Queue#drain        (same Mutex)
#       → FakeTransport.post (discards)
#
# Two knobs make the contention real:
#
#   flush_interval  = 0.001  (1ms) — the flusher wakes ~20 times
#                                    during a 20ms measurement window,
#                                    so drain calls interleave with push
#   queue_size      = 50_000 — larger than ITERATIONS, so events never
#                              drop silently during the measurement
#
# Same 50us P95 ceiling as the NullSink bench. If this bench fails,
# the middleware → client → queue drain path has regressed — tune
# the code, not the threshold. The test body additionally asserts
# queue.dropped == 0 so a "silently fast because we threw data away"
# outcome is caught loudly.

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
      c.flush_interval  = 0.001  # 1ms — flusher drains during measurement
      c.flush_threshold = 100
      c.queue_size      = 50_000 # > ITERATIONS, so no silent drops
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
    begin
      bare         = REFERENCE_APP
      instrumented = Beacon::Middleware.new(REFERENCE_APP, sink: @client, config: Beacon.config)

      request = Rack::MockRequest.env_for("/dashboard", method: "GET")

      bare_p95         = measure_p95(bare, request)
      instrumented_p95 = measure_p95(instrumented, request)

      delta_ns = instrumented_p95 - bare_p95

      warn "[bench queue] bare P95=#{bare_p95}ns instrumented P95=#{instrumented_p95}ns " \
           "delta=#{delta_ns}ns (limit #{MAX_OVERHEAD_NS}ns)"

      # A silently-fast run that achieved its number by overflowing
      # the bounded queue is a lie. Assert explicitly that every
      # event landed, before trusting the delta.
      assert_equal 0, @client.queue.dropped,
        "Bench dropped #{@client.queue.dropped} events — the bounded queue " \
        "overflowed during measurement. Increase queue_size or rework the " \
        "drain cadence; a 'fast' number built on dropped events is meaningless."

      assert_operator delta_ns, :<=, MAX_OVERHEAD_NS,
        "Beacon middleware+Client+Queue added #{delta_ns}ns to P95 " \
        "(limit #{MAX_OVERHEAD_NS}ns) UNDER REAL DRAIN CONTENTION. " \
        "Bare P95: #{bare_p95}ns. Instrumented P95: #{instrumented_p95}ns. " \
        "This is the full production hot path with the flusher actively " \
        "draining the queue at 1ms cadence — the regression is in the " \
        "Middleware, Client, or Queue drain path."
    ensure
      # Thread hygiene: even if the test raises mid-run, the flusher
      # thread must not leak into the process lifetime.
      @client&.shutdown
    end
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
