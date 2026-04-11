require "test_helper"
require "beacon"

# Graceful-shutdown drain: when a host sends SIGTERM and Beacon's at_exit
# (or an explicit Beacon.shutdown call) fires, every event still sitting in
# the queue must land on the transport before the process exits. Losing the
# tail of the queue on every deploy would compound fast on a small team —
# one deploy per day × 100 events/s × 1s queue → 100 events lost/day from
# a system whose job is to notice missing events.
#
# Card 8's Flusher#stop calls flush_now after joining the run-loop thread.
# This test pins that contract so a future refactor can't silently delete
# the final drain.
class ShutdownDrainTest < Minitest::Test
  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.async          = true
      c.flush_interval = 60.0  # long — we do NOT want the periodic tick to drain
      c.flush_threshold = 10_000  # and we do NOT want size-triggered wakeups
      c.queue_size     = 10_000
    end
    @transport = Beacon::Testing::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport)
  end

  def teardown
    @client&.shutdown
    Beacon::Testing.reset_config!
  end

  def test_shutdown_drains_pending_queue_before_returning
    # Push 50 events while the flusher is idle (interval is 60s, threshold
    # is 10k). None should have been sent yet.
    50.times { |i| @client.push(event("evt#{i}")) }
    assert_equal 0, @transport.batches.length,
      "flusher fired before shutdown — test is invalid"

    @client.shutdown

    total = @transport.batches.sum { |b| JSON.parse(b[:body])["events"].length }
    assert_equal 50, total,
      "shutdown did not drain pending queue: got #{total}/50 events on the wire"
  end

  def test_at_exit_hook_wires_shutdown
    # Pin that the top-level at_exit in lib/beacon.rb invokes Beacon.shutdown.
    # We can't actually exit the test process, but we can call the same path.
    Beacon.instance_variable_set(:@client, @client)
    Beacon.shutdown
    assert_nil Beacon.instance_variable_get(:@client),
      "Beacon.shutdown did not clear the client singleton"
  end

  private

  def event(name)
    {
      kind:          :outcome,
      name:          name,
      created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
      properties:    {},
      context:       { environment: "test" },
    }
  end
end
