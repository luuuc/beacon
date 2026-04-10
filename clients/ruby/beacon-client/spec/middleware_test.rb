require "test_helper"
require "rack/mock"
require "beacon/middleware"
require "beacon/test/null_sink"

class MiddlewareTest < Minitest::Test
  OK_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    Beacon.reset_config!
    Beacon.configure do |c|
      c.environment = "test"
      c.deploy_sha  = "deadbeef"
    end
    @sink = Beacon::Test::NullSink.new(record: true)
  end

  def test_perf_event_emitted_with_normalized_name
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/users/42", method: "GET")
    status, _headers, _body = mw.call(env)

    assert_equal 200, status
    assert_equal 1, @sink.events.length
    event = @sink.events.first
    assert_equal :perf, event[:kind]
    assert_equal "GET /users/:id", event[:name]
    assert_equal 200, event[:properties][:status]
    assert_kind_of Integer, event[:properties][:duration_ms]
    assert_equal "test",     event[:context][:environment]
    assert_equal "deadbeef", event[:context][:deploy_sha]
    assert_equal "ruby",     event[:context][:language]
  end

  def test_route_template_short_circuits_normalizer
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/users/42", method: "GET")
    env["beacon.route_template"] = "/users/:user_id"
    mw.call(env)
    assert_equal "GET /users/:user_id", @sink.events.first[:name]
  end

  def test_query_string_does_not_appear_in_name
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/search?q=foo", method: "GET")
    mw.call(env)
    assert_equal "GET /search", @sink.events.first[:name]
  end

  def test_exception_is_recorded_and_re_raised
    boom = ->(_env) { raise NoMethodError, "undefined method" }
    mw   = Beacon::Middleware.new(boom, sink: @sink)
    env  = Rack::MockRequest.env_for("/x", method: "GET")

    assert_raises(NoMethodError) { mw.call(env) }

    kinds = @sink.events.map { |e| e[:kind] }
    assert_includes kinds, :perf
    assert_includes kinds, :error

    error = @sink.events.find { |e| e[:kind] == :error }
    assert_equal "NoMethodError", error[:name]
    assert_equal 40, error[:properties][:fingerprint].length  # SHA1 hex
    assert_equal "undefined method", error[:properties][:message]
  end

  def test_full_stack_only_sent_once_per_fingerprint_per_hour
    boom = ->(_env) { raise RuntimeError, "boom" }
    mw   = Beacon::Middleware.new(boom, sink: @sink)
    env  = Rack::MockRequest.env_for("/x", method: "GET")

    2.times { assert_raises(RuntimeError) { mw.call(env) } }

    errors = @sink.events.select { |e| e[:kind] == :error }
    assert_equal 2, errors.length
    assert errors[0][:properties].key?(:stack_trace), "first occurrence carries stack"
    refute errors[1][:properties].key?(:stack_trace), "second occurrence omits stack"
  end

  def test_message_is_truncated_to_500_chars
    long = "x" * 1000
    boom = ->(_env) { raise RuntimeError, long }
    mw   = Beacon::Middleware.new(boom, sink: @sink)
    env  = Rack::MockRequest.env_for("/x", method: "GET")
    assert_raises(RuntimeError) { mw.call(env) }
    error = @sink.events.find { |e| e[:kind] == :error }
    assert_equal 500, error[:properties][:message].length
  end

  def test_sink_failure_does_not_raise_into_host
    bomb_sink = Object.new
    def bomb_sink.<<(_event); raise "sink exploded"; end
    mw  = Beacon::Middleware.new(OK_APP, sink: bomb_sink)
    env = Rack::MockRequest.env_for("/", method: "GET")
    status, _h, _b = mw.call(env)
    assert_equal 200, status  # host is unaffected
  end

  def test_perf_pillar_can_be_disabled
    Beacon.config.pillars = %i[errors outcomes]
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/", method: "GET")
    mw.call(env)
    assert_equal 0, @sink.events.length
  end
end
