require "test_helper"
require "rack/mock"
require "beacon"
require "beacon/middleware"

# Card 6: Configuration#enabled / BEACON_DISABLED / nil endpoint must
# degrade gracefully. Disabled Beacon never starts a flusher, never
# touches the network, and the middleware becomes a pure passthrough.
class KillSwitchTest < Minitest::Test
  OK_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    @orig_disabled = ENV["BEACON_DISABLED"]
    ENV.delete("BEACON_DISABLED")
    Beacon::Testing.reset_config!
  end

  def teardown
    if @orig_disabled
      ENV["BEACON_DISABLED"] = @orig_disabled
    else
      ENV.delete("BEACON_DISABLED")
    end
    Beacon::Testing.reset_config!
  end

  # --- Configuration.enabled? --------------------------------------------

  def test_enabled_by_default
    assert Beacon.config.enabled?
  end

  def test_explicit_disable
    Beacon.configure { |c| c.enabled = false }
    refute Beacon.config.enabled?
  end

  def test_env_disable_takes_effect_at_config_construction
    ENV["BEACON_DISABLED"] = "1"
    Beacon::Testing.reset_config!
    refute Beacon.config.enabled?
  end

  def test_env_disable_accepts_truthy_values
    %w[1 true yes on TRUE YES ON].each do |val|
      ENV["BEACON_DISABLED"] = val
      Beacon::Testing.reset_config!
      refute Beacon.config.enabled?, "BEACON_DISABLED=#{val.inspect} should disable"
    end
  end

  def test_env_disable_rejects_falsy_values
    %w[0 false no off].each do |val|
      ENV["BEACON_DISABLED"] = val
      Beacon::Testing.reset_config!
      assert Beacon.config.enabled?, "BEACON_DISABLED=#{val.inspect} should not disable"
    end
  end

  def test_nil_endpoint_makes_config_unusable
    Beacon.configure { |c| c.endpoint = nil }
    refute Beacon.config.enabled?
  end

  def test_unparseable_endpoint_makes_config_unusable
    Beacon.configure { |c| c.endpoint = "http:// not a url" }
    refute Beacon.config.enabled?
  end

  # --- Middleware behavior when disabled ---------------------------------

  def test_disabled_middleware_is_pure_passthrough
    Beacon.configure { |c| c.enabled = false }
    bomb_sink = Object.new
    def bomb_sink.<<(_event); raise "sink must not be called"; end
    mw  = Beacon::Middleware.new(OK_APP, sink: bomb_sink)
    env = Rack::MockRequest.env_for("/x", method: "GET")
    status, _h, body = mw.call(env)
    assert_equal 200, status
    assert_equal ["ok"], body
  end

  def test_disabled_middleware_does_not_reraise_host_exceptions_into_beacon
    Beacon.configure { |c| c.enabled = false }
    boom = ->(_env) { raise NoMethodError, "host bug" }
    mw   = Beacon::Middleware.new(boom, sink: nil)
    env  = Rack::MockRequest.env_for("/x", method: "GET")
    assert_raises(NoMethodError) { mw.call(env) }
  end

  # --- Client behavior when disabled -------------------------------------

  def test_disabled_client_does_not_start_flusher_thread
    Beacon.configure { |c| c.enabled = false; c.async = true }
    client = Beacon::Client.new(config: Beacon.config)
    assert_nil client.flusher, "disabled client must not start a flusher"
    refute client.enabled?
    client.shutdown
  end

  def test_disabled_track_returns_nil_and_does_not_enqueue
    Beacon.configure { |c| c.enabled = false; c.async = false }
    client = Beacon::Client.new(config: Beacon.config, autostart: false)
    assert_nil client.track("ignored", k: 1)
    assert_equal 0, client.queue.length
    client.shutdown
  end

  def test_disabled_client_builds_no_transport
    Beacon.configure { |c| c.enabled = false }
    client = Beacon::Client.new(config: Beacon.config, autostart: false)
    assert_nil client.instance_variable_get(:@transport)
    client.shutdown
  end

  # --- Nil endpoint warns once then degrades -----------------------------

  def test_nil_endpoint_logs_single_warning_from_configure
    Beacon::Testing.reset_config!
    out = capture_stderr do
      Beacon.configure { |c| c.endpoint = nil }
    end
    assert_match(/\[beacon\] endpoint is missing or unparseable/, out)
  end

  def test_nil_endpoint_no_warnings_during_requests
    Beacon::Testing.reset_config!
    capture_stderr do
      Beacon.configure { |c| c.endpoint = nil }
    end
    # Subsequent 1000 "requests" through a disabled middleware must
    # not emit any further warnings.
    mw  = Beacon::Middleware.new(OK_APP, sink: nil)
    out = capture_stderr do
      1000.times do
        mw.call(Rack::MockRequest.env_for("/", method: "GET"))
      end
    end
    assert_equal "", out
  end

  # --- Thread-safe singleton init ----------------------------------------

  def test_racing_threads_produce_exactly_one_client_and_one_flusher
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.endpoint = "http://127.0.0.1:1"  # valid URI, flusher will fail silently
      c.async    = true
    end
    clients = []
    threads = Array.new(16) { Thread.new { clients << Beacon.client } }
    threads.each(&:join)
    assert_equal 1, clients.uniq.length,
      "expected exactly one Client across 16 racing threads, got #{clients.uniq.length}"
    assert_equal 1, Thread.list.count { |t| t.name == "beacon-flusher" },
      "expected exactly one beacon-flusher thread across the session"
  end

  private

  def capture_stderr
    original = $stderr
    $stderr  = StringIO.new
    yield
    $stderr.string
  ensure
    $stderr = original
  end
end
