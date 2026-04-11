require "test_helper"
require "beacon"
require "beacon/middleware"

# Load-shed integration test: when the Beacon queue is full AND the
# flusher is dead (stuck, crashed, OOM'd, paused circuit, whatever),
# Rack middleware must stay fast and must not raise into the host.
#
# "Dead flusher + full queue" is the nightmare path — it's the one where
# naive observability gems quietly turn a downstream outage into a
# frontend outage. This test pins three invariants at that worst case:
#
#   1. host app's return value is preserved
#   2. middleware never re-raises Beacon errors
#   3. per-request overhead stays below a strict ceiling
#      (we use 1 ms here — the real-Queue bench enforces the tight
#      50 µs number separately on a warm JIT)
class LoadShedTest < Minitest::Test
  OVERHEAD_CEILING_MS = 1.0
  WARMUP_REQUESTS     = 20
  MEASURED_REQUESTS   = 200

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.async          = false   # no background thread: flusher is "dead"
      c.queue_size     = 10      # tiny queue so it fills after 10 pushes
      c.flush_interval = 60.0
      c.flush_threshold = 9
      c.enabled        = true
    end
    # Sink is a real Beacon::Queue (bounded, oldest-drop) — the same
    # object the middleware would hand events to in production when
    # the flusher is wedged.
    @queue = Beacon::Queue.new(max: 10, flush_threshold: 9)
    sink = QueueSink.new(@queue)

    app = ->(_env) { [200, { "Content-Type" => "text/plain" }, ["ok"]] }
    @middleware = Beacon::Middleware.new(app, sink: sink, config: Beacon.config)
  end

  def teardown
    Beacon::Testing.reset_config!
  end

  def test_full_queue_does_not_raise_or_corrupt_response
    # Overfill: push 50 events into a queue of 10. Oldest-drop kicks in,
    # dropped counter climbs, nothing raises.
    50.times do
      status, _headers, body = @middleware.call(rack_env)
      assert_equal 200, status
      assert_equal ["ok"], body
    end
    assert_operator @queue.dropped, :>=, 40,
      "expected ≥40 drops with queue=10 and 50 pushes, got #{@queue.dropped}"
    assert_equal 10, @queue.length,
      "queue should be pinned at max after overflow"
  end

  def test_host_exception_is_re_raised_even_when_queue_is_full
    # Fill the queue first.
    15.times { @middleware.call(rack_env) }

    boom_app = ->(_env) { raise "boom" }
    mw = Beacon::Middleware.new(boom_app,
      sink: QueueSink.new(@queue),
      config: Beacon.config)

    err = assert_raises(RuntimeError) { mw.call(rack_env) }
    assert_equal "boom", err.message
  end

  def test_middleware_overhead_ceiling_under_load_shed
    # Fill the queue so every subsequent push hits the drop path.
    15.times { @middleware.call(rack_env) }
    assert_equal 10, @queue.length

    WARMUP_REQUESTS.times { @middleware.call(rack_env) }

    # Subtract out the baseline app call so we measure Beacon's overhead
    # rather than Process.clock_gettime itself.
    baseline_app = ->(_env) { [200, {}, []] }
    baseline_mw  = BypassWrapper.new(baseline_app)
    WARMUP_REQUESTS.times { baseline_mw.call(rack_env) }

    beacon_ms  = measure(@middleware)
    baseline_ms = measure(baseline_mw)
    overhead_ms = beacon_ms - baseline_ms

    assert_operator overhead_ms, :<, OVERHEAD_CEILING_MS,
      "Beacon middleware overhead = %.3f ms (baseline %.3f, beacon %.3f) " \
      "exceeded %.3f ms ceiling under load-shed" %
      [overhead_ms, baseline_ms, beacon_ms, OVERHEAD_CEILING_MS]
  end

  private

  def measure(handler)
    start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    MEASURED_REQUESTS.times { handler.call(rack_env) }
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - start
    (elapsed * 1_000.0) / MEASURED_REQUESTS
  end

  def rack_env
    {
      "REQUEST_METHOD" => "GET",
      "PATH_INFO"      => "/widgets/42",
      "QUERY_STRING"   => "",
      "rack.input"     => StringIO.new(""),
    }
  end

  # Minimal sink wrapping a Beacon::Queue — the queue is what the real
  # Beacon::Client stores events in, so this is the production drop
  # path, just without the background flusher.
  class QueueSink
    def initialize(queue)
      @queue = queue
    end

    def push(event)
      @queue.push(event)
    end
    alias << push
  end

  # Minimal middleware-shaped wrapper used for baseline timing.
  class BypassWrapper
    def initialize(app); @app = app; end
    def call(env); @app.call(env); end
  end
end
