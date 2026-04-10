require "test_helper"
require "beacon"

class StatsTest < Minitest::Test
  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.async          = false
      c.flush_interval = 0.05
    end
    @transport = Beacon::Testing::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport, autostart: false)
    # Swap the global client so Beacon.stats reads our instance.
    Beacon.instance_variable_set(:@client, @client)
  end

  def teardown
    @client&.shutdown
    Beacon::Testing.reset_config!
  end

  def test_stats_returns_documented_shape
    stats = Beacon.stats
    %i[queue_depth queue_max dropped sent last_flush_at last_flush_status
       circuit_open consecutive_failures reconnects enabled].each do |key|
      assert stats.key?(key), "expected Beacon.stats to include #{key}"
    end
  end

  def test_stats_reflects_queue_depth_and_sent_counter
    3.times { |i| @client.track("e#{i}") }
    assert_equal 3, Beacon.stats[:queue_depth]

    flusher = Beacon::Flusher.new(@client, transport: @transport)
    @client.instance_variable_set(:@flusher, flusher)
    flusher.flush_now
    stats = Beacon.stats
    assert_equal 3, stats[:sent]
    assert_equal :ok, stats[:last_flush_status]
    assert_kind_of Time, stats[:last_flush_at]
    assert_equal 0, stats[:queue_depth]
  end

  def test_stats_when_disabled
    Beacon::Testing.reset_config!
    Beacon.configure { |c| c.enabled = false }
    Beacon.instance_variable_set(:@client, Beacon::Client.new(config: Beacon.config, autostart: false))
    stats = Beacon.stats
    refute stats[:enabled]
    assert_equal 0, stats[:sent]
  end
end
