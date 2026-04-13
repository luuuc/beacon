require "test_helper"
require "stringio"

# We don't boot Rails for this test. We stub just enough of Rails::Railtie's
# surface that beacon/rails can be required and its initializer blocks can be
# invoked against a fake application.
module FakeRails
  class FakeMiddlewareStack
    attr_reader :ops
    def initialize; @ops = []; end
    def use(klass, *args); @ops << [:use, klass, args]; end
    def insert_after(anchor, klass, *args); @ops << [:insert_after, anchor, klass, args]; end
    def insert_before(anchor, klass, *args); @ops << [:insert_before, anchor, klass, args]; end
    def used; @ops.select { |op| op[0] == :use }.map { |_, k, a| [k, a] }; end
  end

  class FakeApp
    def middleware; @middleware ||= FakeMiddlewareStack.new; end
  end

  module RailtieShim
    def self.included(base)
      base.extend(ClassMethods)
    end

    module ClassMethods
      def initializer(name, opts = {}, &block)
        initializers << [name, opts, block]
      end

      def initializers
        @initializers ||= []
      end

      def config
        @config ||= FakeConfig.new
      end

      # Stub for Rails::Railtie#rake_tasks. Real Rails calls the
      # block when `rake` discovers tasks; our shim just stores the
      # block so the Railtie can be loaded under the fake. Coverage
      # for actual rake-task loading lives in spec/rake_tasks_test.rb.
      def rake_tasks(&block)
        (@rake_task_blocks ||= []) << block if block
      end

      def rake_task_blocks
        @rake_task_blocks ||= []
      end
    end

    class FakeConfig
      def before_configuration(&block)
        @before_configuration_blocks ||= []
        @before_configuration_blocks << block
      end

      def before_configuration_blocks
        @before_configuration_blocks ||= []
      end

      def after_initialize(&block)
        @after_initialize_blocks ||= []
        @after_initialize_blocks << block
      end

      def after_initialize_blocks
        @after_initialize_blocks ||= []
      end
    end
  end
end

# Install the stub under the ::Rails::Railtie name the gem expects.
unless defined?(::Rails::Railtie)
  Object.const_set(:Rails, Module.new) unless defined?(::Rails)
  ::Rails.const_set(:Railtie, Class.new { include FakeRails::RailtieShim })
  ::Rails.define_singleton_method(:root) { Pathname.new(Dir.pwd) } unless ::Rails.respond_to?(:root)
  ::Rails.define_singleton_method(:application) { nil } unless ::Rails.respond_to?(:application)
end

require "pathname"
require "beacon/rails"

class RailsRailtieTest < Minitest::Test
  def setup
    Beacon::Testing.reset_config!
  end

  def teardown
    Beacon::Testing.reset_config!
  end

  def test_railtie_registers_expected_initializers
    names = Beacon::Railtie.initializers.map(&:first)
    assert_includes names, "beacon.insert_middleware"
    assert_includes names, "beacon.install_integrations"
    assert_includes names, "beacon.install_fork_hook"
  end

  def test_insert_middleware_uses_bare_use_when_debug_exceptions_not_loaded
    app = FakeRails::FakeApp.new
    initializer_block("beacon.insert_middleware").call(app)
    assert_equal [[:use, Beacon::Middleware, []]], app.middleware.ops
  end

  def test_insert_middleware_pins_insertion_after_debug_exceptions
    # Stub ActionDispatch::DebugExceptions so the initializer takes the
    # Rails-aware branch and uses insert_after.
    unless defined?(::ActionDispatch::DebugExceptions)
      Object.const_set(:ActionDispatch, Module.new) unless defined?(::ActionDispatch)
      ::ActionDispatch.const_set(:DebugExceptions, Class.new)
      @stubbed_debug_exceptions = true
    end
    app = FakeRails::FakeApp.new
    initializer_block("beacon.insert_middleware").call(app)
    op = app.middleware.ops.first
    assert_equal :insert_after, op[0]
    assert_equal ::ActionDispatch::DebugExceptions, op[1]
    assert_equal Beacon::Middleware, op[2]
  ensure
    if @stubbed_debug_exceptions
      ::ActionDispatch.send(:remove_const, :DebugExceptions)
      @stubbed_debug_exceptions = false
    end
  end

  def test_insert_middleware_is_skipped_when_perf_and_errors_disabled
    Beacon.config.pillars = [:outcomes]
    app = FakeRails::FakeApp.new
    initializer_block("beacon.insert_middleware").call(app)
    assert_empty app.middleware.ops
  end

  def test_install_integrations_initializer_invokes_active_job_and_action_mailer
    # Pre-require the integration files so the initializer's `require`
    # calls become no-ops, then spy on .install.
    require "beacon/integrations/active_job"
    require "beacon/integrations/action_mailer"

    unless defined?(::ActiveJob)
      Object.const_set(:ActiveJob, Module.new)
      @stubbed_aj = true
    end
    unless defined?(::ActionMailer)
      Object.const_set(:ActionMailer, Module.new)
      @stubbed_am = true
    end

    calls = []
    aj_orig = Beacon::Integrations::ActiveJob.method(:install)
    am_orig = Beacon::Integrations::ActionMailer.method(:install)
    Beacon::Integrations::ActiveJob.define_singleton_method(:install)    { |**_| calls << :active_job }
    Beacon::Integrations::ActionMailer.define_singleton_method(:install) { |**_| calls << :action_mailer }

    initializer_block("beacon.install_integrations").call(FakeRails::FakeApp.new)
    assert_equal %i[active_job action_mailer], calls
  ensure
    Beacon::Integrations::ActiveJob.define_singleton_method(:install,    aj_orig) if aj_orig
    Beacon::Integrations::ActionMailer.define_singleton_method(:install, am_orig) if am_orig
    Object.send(:remove_const, :ActiveJob)    if @stubbed_aj && Object.const_defined?(:ActiveJob, false)
    Object.send(:remove_const, :ActionMailer) if @stubbed_am && Object.const_defined?(:ActionMailer, false)
  end

  def test_fork_hook_module_runs_after_fork_in_child
    with_fake_beacon_client do |calls|
      host = Object.new
      host.define_singleton_method(:_fork) { 0 }  # pretend we're in the child
      host.singleton_class.prepend(Beacon::Railtie::ForkHook)
      host._fork
      assert_equal [:after_fork], calls
    end
  end

  def test_fork_hook_does_not_run_after_fork_in_parent
    with_fake_beacon_client do |calls|
      host = Object.new
      host.define_singleton_method(:_fork) { 12345 }  # parent: non-zero pid
      host.singleton_class.prepend(Beacon::Railtie::ForkHook)
      assert_equal 12345, host._fork
      assert_empty calls
    end
  end

  def test_route_template_for_reads_request_route_uri_pattern
    env = { "REQUEST_METHOD" => "GET" }
    request = fake_request(env, route_uri_pattern: "/users/:id(.:format)")
    assert_equal "GET /users/:id",
      Beacon::Railtie.route_template_for(env, { request: request })
  end

  def test_route_template_for_passes_through_when_no_format_suffix
    env = { "REQUEST_METHOD" => "GET" }
    request = fake_request(env, route_uri_pattern: "/health")
    assert_equal "GET /health",
      Beacon::Railtie.route_template_for(env, { request: request })
  end

  def test_route_template_for_only_strips_trailing_format_suffix
    env = { "REQUEST_METHOD" => "GET" }
    request = fake_request(env, route_uri_pattern: "/api(/:version)/users/:id(.:format)")
    assert_equal "GET /api(/:version)/users/:id",
      Beacon::Railtie.route_template_for(env, { request: request })
  end

  def test_route_template_for_returns_nil_when_request_missing
    assert_nil Beacon::Railtie.route_template_for({ "REQUEST_METHOD" => "GET" }, {})
  end

  def test_route_template_for_returns_nil_when_pattern_nil
    env = { "REQUEST_METHOD" => "GET" }
    request = fake_request(env, route_uri_pattern: nil)
    assert_nil Beacon::Railtie.route_template_for(env, { request: request })
  end

  def test_route_template_for_returns_nil_when_request_does_not_respond_to_accessor
    # Pre-7.1 Rails: request does not respond to route_uri_pattern at all.
    env = { "REQUEST_METHOD" => "GET" }
    bare_request = Object.new
    bare_request.define_singleton_method(:env) { env }
    assert_nil Beacon::Railtie.route_template_for(env, { request: bare_request })
  end

  def test_subscribe_action_controller_writes_route_template_into_env
    with_stubbed_notifications do |captured_ref|
      initializer_block("beacon.subscribe_action_controller").call(FakeRails::FakeApp.new)
      captured = captured_ref.call
      refute_nil captured, "initializer should have subscribed"

      env = { "REQUEST_METHOD" => "GET" }
      request = fake_request(env, route_uri_pattern: "/orders/:id(.:format)")
      captured.call("start_processing.action_controller", nil, nil, nil,
        { request: request, controller: "OrdersController", action: "show" })

      assert_equal "GET /orders/:id", env["beacon.route_template"]
    end
  end

  def test_subscribe_action_controller_swallows_exceptions_from_bad_payload
    with_stubbed_notifications do |captured_ref|
      initializer_block("beacon.subscribe_action_controller").call(FakeRails::FakeApp.new)
      captured = captured_ref.call

      # Request whose #env raises — simulates a torn-down request or a
      # weird host injecting a broken payload.
      exploding = Object.new
      exploding.define_singleton_method(:env) { raise "kaboom" }

      orig_stderr = $stderr
      $stderr = StringIO.new
      begin
        # Must not raise into the notifier loop.
        captured.call("start_processing.action_controller", nil, nil, nil,
          { request: exploding })
        assert_match(/\[beacon\] action_controller subscriber rescued/, $stderr.string)
      ensure
        $stderr = orig_stderr
      end
    end
  end

  def test_auto_fires_deploy_shipped_when_deploy_sha_present
    Beacon.config.deploy_sha = "abc123"
    Beacon.config.async = false
    Beacon::Testing.reset_client!

    after_init = Beacon::Railtie.config.after_initialize_blocks.last
    refute_nil after_init, "Railtie should register an after_initialize block"
    after_init.call

    events = Beacon.client.queue.drain(100)
    deploy_event = events.find { |e| e[:name] == "deploy.shipped" }
    refute_nil deploy_event, "should have fired deploy.shipped"
    assert_equal "abc123", deploy_event[:properties][:version]
    assert_equal true, deploy_event[:properties][:auto]
  end

  def test_does_not_fire_deploy_shipped_when_no_sha
    Beacon.config.deploy_sha = nil
    Beacon.config.async = false
    Beacon::Testing.reset_client!

    after_init = Beacon::Railtie.config.after_initialize_blocks.last
    after_init.call

    events = Beacon.client.queue.drain(100)
    deploy_event = events.find { |e| e[:name] == "deploy.shipped" }
    assert_nil deploy_event, "should not fire deploy.shipped without a SHA"
  end

  def test_middleware_defaults_sink_to_beacon_client
    require "rack/mock"
    Beacon.config.async = false  # no flusher thread for this test
    Beacon::Testing.reset_client!
    app = ->(_env) { [200, {}, ["ok"]] }
    mw  = Beacon::Middleware.new(app)  # no sink: → resolves to Beacon.client
    before = Beacon.client.queue.length
    mw.call(Rack::MockRequest.env_for("/", method: "GET"))
    assert_equal before + 1, Beacon.client.queue.length,
      "middleware should have pushed one event into Beacon.client's queue"
  end

  private

  def initializer_block(name)
    Beacon::Railtie.initializers.find { |n, _, _| n == name }.last
  end

  # Build a fake ActionDispatch::Request-shaped object. Exposes #env and
  # #route_uri_pattern (the Rails 7.1+ accessor Beacon reads).
  def fake_request(env, route_uri_pattern:)
    request = Object.new
    request.define_singleton_method(:env) { env }
    request.define_singleton_method(:route_uri_pattern) { route_uri_pattern }
    request
  end

  # Temporarily replace ::ActiveSupport::Notifications with a stub that
  # captures the block passed to .subscribe. The test body can invoke the
  # captured block to simulate a notification firing, without pulling the
  # full activesupport gem into the test environment.
  def with_stubbed_notifications
    captured = nil
    had_active_support  = defined?(::ActiveSupport)
    had_notifications   = had_active_support && ::ActiveSupport.const_defined?(:Notifications, false)
    orig_notifications  = had_notifications ? ::ActiveSupport::Notifications : nil

    Object.const_set(:ActiveSupport, Module.new) unless had_active_support
    ::ActiveSupport.send(:remove_const, :Notifications) if had_notifications
    stub = Module.new
    stub.define_singleton_method(:subscribe) { |_name, &block| captured = block }
    ::ActiveSupport.const_set(:Notifications, stub)

    yield(-> { captured })
  ensure
    if ::ActiveSupport.const_defined?(:Notifications, false)
      ::ActiveSupport.send(:remove_const, :Notifications)
    end
    ::ActiveSupport.const_set(:Notifications, orig_notifications) if had_notifications
    Object.send(:remove_const, :ActiveSupport) if !had_active_support && defined?(::ActiveSupport)
  end

  # Temporarily replace Beacon.client with a stub that records after_fork
  # calls into the block-yielded array.
  def with_fake_beacon_client
    calls = []
    fake = Object.new
    fake.define_singleton_method(:after_fork) { calls << :after_fork }
    orig = Beacon.method(:client)
    Beacon.define_singleton_method(:client) { fake }
    yield calls
  ensure
    Beacon.define_singleton_method(:client, orig) if orig
  end
end
