require "test_helper"
require "stringio"
require "beacon"
require "beacon/test/fake_transport"

class FlusherTest < Minitest::Test
  ZERO_BACKOFF = [0, 0, 0, 0, 0].freeze

  def setup
    Beacon.reset_config!
    Beacon.configure do |c|
      c.async          = false
      c.flush_interval = 0.05
    end
    @transport = Beacon::Test::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport, autostart: false)
    @flusher   = Beacon::Flusher.new(@client, transport: @transport, backoff: ZERO_BACKOFF)
  end

  def teardown
    @flusher.stop
    @client.shutdown
    Beacon.reset_config!
  end

  def test_flush_now_drains_queue_and_serializes_to_json
    @client.track("signup.completed", plan: "pro")
    @flusher.flush_now

    assert_equal 1, @transport.batches.length
    payload = JSON.parse(@transport.batches.first[:body])
    assert_equal 1, payload["events"].length

    event = payload["events"].first
    assert_equal "outcome", event["kind"]
    assert_equal "signup.completed", event["name"]
    assert_match(/\A\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z\z/, event["created_at"])
    assert_equal({ "plan" => "pro" }, event["properties"])
    assert_equal "ruby", event["context"]["language"]
  end

  def test_idempotency_key_is_uuid
    @client.track("e")
    @flusher.flush_now
    key = @transport.batches.first[:idempotency_key]
    assert_match(/\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/, key)
  end

  def test_4xx_drops_batch_without_retry
    @client.track("e")
    @transport.respond_with(status: 400)
    silence_warnings { @flusher.flush_now }
    assert_equal 1, @transport.batches.length  # one attempt only
  end

  def test_5xx_retries_with_backoff_then_drops
    @client.track("e")
    6.times { @transport.respond_with(status: 503) }
    silence_warnings { @flusher.flush_now }
    assert_equal 5, @transport.batches.length
  end

  def test_transport_error_retries
    @client.track("e")
    4.times { @transport.respond_with(error: RuntimeError.new("connection refused")) }
    @transport.respond_with(status: 202)
    silence_warnings { @flusher.flush_now }
    assert_equal 5, @transport.batches.length
  end

  def test_circuit_breaker_opens_after_five_consecutive_failed_batches
    5.times do |i|
      @client.track("e#{i}")
      6.times { @transport.respond_with(status: 503) }
      silence_warnings { @flusher.flush_now }
    end
    assert @flusher.instance_variable_get(:@circuit_open_until),
      "circuit should be open after 5 consecutive failures"
    assert_operator @flusher.instance_variable_get(:@consecutive_failures), :>=, 5
  end

  def test_circuit_resets_after_successful_batch
    @client.track("first")
    6.times { @transport.respond_with(status: 503) }
    silence_warnings { @flusher.flush_now }
    assert_equal 1, @flusher.instance_variable_get(:@consecutive_failures)

    @client.track("second")
    @transport.respond_with(status: 202)
    @flusher.flush_now
    assert_equal 0, @flusher.instance_variable_get(:@consecutive_failures)
  end

  def test_event_omits_blank_actor_and_properties
    @client.push({
      kind: :outcome,
      name: "x",
      created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
      properties: {},
    })
    @flusher.flush_now
    event = JSON.parse(@transport.batches.first[:body])["events"].first
    refute event.key?("actor_type")
    refute event.key?("actor_id")
    refute event.key?("properties")
  end

  def test_envelope_matches_fixture_shape
    user = Object.new
    def user.id; 42; end
    def user.class; FakeUserClass; end
    @client.track("signup.completed", user: user, plan: "pro")
    @flusher.flush_now
    event = JSON.parse(@transport.batches.first[:body])["events"].first
    assert_equal "outcome",          event["kind"]
    assert_equal "signup.completed", event["name"]
    assert_equal "FakeUserClass",    event["actor_type"]
    assert_equal 42,                 event["actor_id"]
    assert_equal({ "plan" => "pro" }, event["properties"])
  end

  FakeUserClass = Class.new do
    def self.name; "FakeUserClass"; end
  end

  def test_oversized_flush_splits_into_multiple_batches
    # Drive the body-size splitter with events whose serialized form
    # is big enough that two will exceed BATCH_MAX_BYTES (800 KB). We
    # use ~500 KB of padding per event so any two together blow the
    # cap and must ship in separate POSTs.
    big_string = "x" * 500_000
    3.times do |i|
      @client.push({
        kind: :outcome,
        name: "evt#{i}",
        created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
        properties: { pad: big_string },
      })
    end
    @flusher.flush_now
    # 3 events × 500 KB each → at least 2 separate POSTs (likely 3,
    # because each single event is big enough that adding a second
    # would cross the cap).
    assert_operator @transport.batches.length, :>=, 2,
      "expected body-size splitter to produce multiple batches, got #{@transport.batches.length}"
    # Every batch body must respect the 800 KB ceiling.
    @transport.batches.each do |batch|
      assert_operator batch[:body].bytesize, :<=, Beacon::Flusher::BATCH_MAX_BYTES,
        "batch exceeded BATCH_MAX_BYTES: #{batch[:body].bytesize} bytes"
    end
    # And every event should still be accounted for across all batches.
    total_events = @transport.batches.sum do |batch|
      JSON.parse(batch[:body])["events"].length
    end
    assert_equal 3, total_events
  end

  private

  def silence_warnings
    orig = $stderr
    $stderr = StringIO.new
    yield
  ensure
    $stderr = orig
  end
end
