require "test_helper"
require "rack/mock"
require "beacon/middleware"

class AmbientTest < Minitest::Test
  OK_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.environment = "test"
      c.deploy_sha  = "deadbeef"
    end
    @sink = Beacon::Testing::NullSink.new(record: true)
  end

  # -----------------------------------------------------------------------
  # Ambient mode off (default)
  # -----------------------------------------------------------------------

  def test_ambient_off_emits_only_perf
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/users", method: "GET")
    mw.call(env)

    kinds = @sink.events.map { |e| e[:kind] }
    assert_equal [:perf], kinds
  end

  # -----------------------------------------------------------------------
  # Ambient mode on
  # -----------------------------------------------------------------------

  def test_ambient_on_emits_perf_and_ambient
    Beacon.configure { |c| c.ambient = true }
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/search", method: "GET")
    mw.call(env)

    kinds = @sink.events.map { |e| e[:kind] }
    assert_includes kinds, :perf
    assert_includes kinds, :ambient
    assert_equal 2, @sink.events.length
  end

  def test_ambient_event_has_correct_shape
    Beacon.configure { |c| c.ambient = true }
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/dashboard", method: "POST")
    mw.call(env)

    ambient = @sink.events.find { |e| e[:kind] == :ambient }
    refute_nil ambient
    assert_equal "http_request", ambient[:name]
    assert_equal "/dashboard", ambient[:properties][:path]
    assert_equal "POST", ambient[:properties][:method]
    assert_equal 200, ambient[:properties][:status]
    assert_kind_of Integer, ambient[:properties][:duration_ms]
  end

  def test_ambient_event_carries_enrichment_dimensions
    Beacon.configure do |c|
      c.ambient = true
      c.enrich_context do |request|
        { country: "US" }
      end
    end
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/", method: "GET")
    mw.call(env)

    ambient = @sink.events.find { |e| e[:kind] == :ambient }
    assert_equal({ country: "US" }, ambient[:dimensions])
  end

  def test_ambient_emitted_on_error_path
    Beacon.configure { |c| c.ambient = true }
    boom_app = ->(_env) { raise RuntimeError, "kaboom" }
    mw = Beacon::Middleware.new(boom_app, sink: @sink)
    env = Rack::MockRequest.env_for("/fail", method: "GET")

    assert_raises(RuntimeError) { mw.call(env) }

    kinds = @sink.events.map { |e| e[:kind] }
    assert_includes kinds, :ambient
    ambient = @sink.events.find { |e| e[:kind] == :ambient }
    assert_equal 500, ambient[:properties][:status]
  end
end
