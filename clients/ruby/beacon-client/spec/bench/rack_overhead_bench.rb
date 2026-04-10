# Beacon — Rack overhead benchmark.
#
# This file is the FIRST artifact of the Ruby client cycle. It exists before
# the middleware does, and it stays red until the middleware lands and is
# fast enough.
#
# Contract (from doc/definition/05-clients.md):
#
#   "Target overhead: under 50µs added to P95 request latency on a reference
#    Rack endpoint."
#
# How it works:
#
#   1. Build a trivial reference Rack endpoint (returns "ok").
#   2. Time N requests through the bare endpoint -> baseline P95.
#   3. Wrap the same endpoint in Beacon::Middleware pointed at a discarding
#      sink (no real network I/O — we are measuring the hot path, not the
#      flusher).
#   4. Time N requests through the wrapped endpoint -> instrumented P95.
#   5. Assert: instrumented_p95 - baseline_p95 <= MAX_OVERHEAD_NS.
#
# A failed bench BLOCKS THE BUILD. Tune the middleware, not the threshold.

$LOAD_PATH.unshift File.expand_path("../../lib", __dir__)

require "minitest/autorun"
require "rack/mock"
require "beacon"
require "beacon/middleware"  # intentionally fails until card 14 lands
require "beacon/testing"

class RackOverheadBench < Minitest::Test
  ITERATIONS    = 20_000
  WARMUP        = 2_000
  MAX_OVERHEAD_NS = 50_000  # 50 microseconds

  REFERENCE_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def test_added_p95_under_fifty_microseconds
    bare         = REFERENCE_APP
    instrumented = Beacon::Middleware.new(REFERENCE_APP, sink: Beacon::Testing::NullSink.new)

    request = Rack::MockRequest.env_for("/dashboard", method: "GET")

    bare_p95         = measure_p95(bare, request)
    instrumented_p95 = measure_p95(instrumented, request)

    delta_ns = instrumented_p95 - bare_p95

    warn "[bench] bare P95=#{bare_p95}ns instrumented P95=#{instrumented_p95}ns delta=#{delta_ns}ns (limit #{MAX_OVERHEAD_NS}ns)"

    assert_operator delta_ns, :<=, MAX_OVERHEAD_NS,
      "Beacon middleware added #{delta_ns}ns to P95 (limit #{MAX_OVERHEAD_NS}ns). " \
      "Bare P95: #{bare_p95}ns. Instrumented P95: #{instrumented_p95}ns. " \
      "If this fails, the middleware allocates too much on the hot path."
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
