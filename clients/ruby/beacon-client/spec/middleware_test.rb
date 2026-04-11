require "test_helper"
require "rack/mock"
require "beacon/middleware"

class MiddlewareTest < Minitest::Test
  OK_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.environment = "test"
      c.deploy_sha  = "deadbeef"
    end
    @sink = Beacon::Testing::NullSink.new(record: true)
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
    # The Railtie subscriber stores the full "<METHOD> <template>" shape
    # (same cross-client contract as PathNormalizer). Middleware uses it
    # verbatim — no re-prefixing, no double-method regression.
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/users/42", method: "GET")
    env["beacon.route_template"] = "GET /users/:user_id"
    mw.call(env)
    assert_equal "GET /users/:user_id", @sink.events.first[:name]
  end

  def test_route_template_is_not_double_prefixed
    # v0.2.2 regression guard: pre-fix this test would have returned
    # "GET GET /users/:user_id" because the middleware prepended method
    # on top of a template that already included it. The staging
    # deploy on 2026-04-11 hit this in every single perf rollup.
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/users/42", method: "GET")
    env["beacon.route_template"] = "GET /users/:user_id"
    mw.call(env)
    name = @sink.events.first[:name]
    refute_match(/\A(\w+)\s+\1\s/, name,
      "middleware must not prepend method when template already has one (got #{name.inspect})")
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

  def test_name_cache_stays_bounded_under_distinct_url_probing
    # 10,000 distinct paths simulate a bot probing a Rails app. Before the
    # LRU, this grew @name_cache unbounded (the old size check only counted
    # top-level method keys). Now it must cap at cache_size.
    Beacon.config.cache_size = 512
    mw = Beacon::Middleware.new(OK_APP, sink: @sink)
    10_000.times do |i|
      mw.call(Rack::MockRequest.env_for("/probe/#{i}/leaf", method: "GET"))
    end
    assert_operator mw.stats[:name_cache_size], :<=, 512
  end

  def test_stack_seen_stays_bounded_under_distinct_fingerprints
    # Drive the throttle directly with 500 genuinely distinct fingerprints.
    # The earlier version of this test used `Class.new(StandardError)` at
    # raise time, but anonymous classes have `.name == nil`, which meant
    # every iteration hit the same fingerprint — trivially green even
    # without an LRU.
    Beacon.config.cache_size = 256
    mw = Beacon::Middleware.new(OK_APP, sink: @sink)
    500.times do |i|
      fingerprint = Beacon::Fingerprint.compute("SomeError#{i}", "app/controllers/x.rb")
      mw.send(:should_send_full_stack?, fingerprint)
    end
    assert_operator mw.stats[:stack_seen_size], :<=, 256
    refute_equal 0, mw.stats[:stack_seen_size],
      "sanity check: fingerprints must actually land in the cache"
  end

  def test_stack_trace_is_truncated_to_sixteen_kilobytes
    # A 500-frame synthetic backtrace easily exceeds the 16 KB property
    # limit. Keep as many leading frames as fit, append the truncation
    # marker, never exceed the cap.
    fake_locations = Array.new(500) do |i|
      loc = Object.new
      loc.define_singleton_method(:path)          { "/gems/fake/lib/file#{i}.rb" }
      loc.define_singleton_method(:absolute_path) { nil }
      loc.define_singleton_method(:lineno)        { 10 + i }
      loc.define_singleton_method(:base_label)    { "method_#{i}" }
      loc
    end
    err = RuntimeError.new("boom")
    err.define_singleton_method(:backtrace_locations) { fake_locations }

    mw  = Beacon::Middleware.new(->(_) { raise err }, sink: @sink)
    env = Rack::MockRequest.env_for("/x", method: "GET")
    assert_raises(RuntimeError) { mw.call(env) }

    error = @sink.events.find { |e| e[:kind] == :error }
    stack = error[:properties][:stack_trace]
    refute_nil stack, "expected a stack_trace property"
    assert_operator stack.bytesize, :<=, Beacon::Middleware::STACK_TRACE_MAX_BYTES
    assert stack.end_with?("… (truncated)"), "expected truncation marker"
    assert_includes stack, "file0.rb:10:in `method_0'", "must keep leading frames"
  end

  def test_format_stack_trace_against_real_raise
    # End-to-end: raise a real exception from this file and assert the
    # captured stack_trace contains a line from middleware_test.rb with
    # the expected `:lineno:in `label'` suffix shape. Stubbed
    # backtrace_locations tests prove the formatter; this proves the
    # wiring against real Ruby data.
    Beacon.config.app_root = File.expand_path("..", __dir__)
    mw  = Beacon::Middleware.new(->(_) { raise RuntimeError, "boom" }, sink: @sink)
    env = Rack::MockRequest.env_for("/x", method: "GET")
    assert_raises(RuntimeError) { mw.call(env) }
    error = @sink.events.find { |e| e[:kind] == :error }
    stack = error[:properties][:stack_trace]
    refute_nil stack
    assert_match(/middleware_test\.rb:\d+:in `/, stack,
      "expected a stack frame from middleware_test.rb in the real raise path")
  end

  def test_first_app_frame_uses_backtrace_locations
    # Rather than hand-building a full StandardError with a synthetic
    # backtrace_locations, raise a real exception with a frame from this
    # test file and assert that the captured first_app_frame is an app
    # frame containing this file's path and a line number.
    Beacon.config.app_root = File.expand_path("..", __dir__)  # the gem root
    mw  = Beacon::Middleware.new(->(_) { raise RuntimeError, "boom" }, sink: @sink)
    env = Rack::MockRequest.env_for("/x", method: "GET")
    assert_raises(RuntimeError) { mw.call(env) }
    error = @sink.events.find { |e| e[:kind] == :error }
    frame = error[:properties][:first_app_frame]
    # The first app frame should land in spec/ (the host "app") with a
    # line number suffix in the new `path:lineno` shape.
    assert_match(%r{\Aspec/.+\.rb:\d+\z}, frame,
      "expected app-relative frame with lineno suffix, got #{frame.inspect}")
  end

  def test_middleware_caches_tolerate_concurrent_inserts
    mw = Beacon::Middleware.new(OK_APP, sink: @sink)
    threads = 16
    per_thread = 500
    ts = Array.new(threads) do |t|
      Thread.new do
        per_thread.times do |i|
          env = Rack::MockRequest.env_for("/t#{t}/#{i}", method: "GET")
          mw.call(env)
        end
      end
    end
    ts.each(&:join)
    assert_operator mw.stats[:name_cache_size], :<=, Beacon.config.cache_size
  end
end
