require "test_helper"
require "beacon"

class ClientTest < Minitest::Test
  FakeUser = Struct.new(:id) do
    def self.name; "User"; end
  end

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.environment = "test"
      c.deploy_sha  = "abc123"
      c.async       = false  # don't start a real flusher in tests
    end
    @transport = Beacon::Testing::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport, autostart: false)
  end

  def teardown
    @client.shutdown
    Beacon::Testing.reset_config!
  end

  def test_track_builds_outcome_event
    @client.track("signup.completed", plan: "pro")
    event = @client.queue.drain(1).first

    assert_equal :outcome, event[:kind]
    assert_equal "signup.completed", event[:name]
    assert_equal({ plan: "pro" }, event[:properties])
    assert_equal "test",   event[:context][:environment]
    assert_equal "abc123", event[:context][:deploy_sha]
    assert_equal "ruby",   event[:context][:language]
    assert_kind_of Integer, event[:created_at_ns]
  end

  def test_user_shorthand_extracts_actor
    # Integer IDs are stringified so the single actor_id code path
    # handles both legacy integer PKs and modern UUID PKs uniformly.
    user = FakeUser.new(42)
    @client.track("signup.completed", user: user, plan: "pro")
    event = @client.queue.drain(1).first

    assert_equal "User", event[:actor_type]
    assert_equal "42",   event[:actor_id]
    assert_equal({ plan: "pro" }, event[:properties])
    refute event[:properties].key?(:user)
  end

  def test_user_shorthand_passes_uuid_through_unchanged
    # v0.2.0 contract: Rails 7.1+ UUID primary keys land in actor_id
    # as-is. No parsing, no validation — Beacon's server accepts any
    # string up to 128 chars.
    user = FakeUser.new("019245ab-d36e-7000-8000-000000000001")
    @client.track("user.signed_up", user: user)
    event = @client.queue.drain(1).first

    assert_equal "User", event[:actor_type]
    assert_equal "019245ab-d36e-7000-8000-000000000001", event[:actor_id]
  end

  def test_track_does_not_mutate_caller_hash
    props = { user: FakeUser.new(1), plan: "pro" }
    @client.track("e", props)
    assert props.key?(:user), "caller's hash must not be mutated"
  end

  def test_outcomes_pillar_disabled
    Beacon.config.pillars = [:perf, :errors]
    @client.track("ignored", k: 1)
    assert_equal 0, @client.queue.length
  end

  def test_track_failure_is_swallowed
    bad = Object.new
    def bad.dup; raise "boom"; end
    assert_nil @client.track("e", bad)
  end
end
