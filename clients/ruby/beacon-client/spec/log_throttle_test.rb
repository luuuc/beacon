require "test_helper"
require "stringio"
require "beacon/log_throttle"

class LogThrottleTest < Minitest::Test
  def setup
    @clock_now = 0.0
    @clock     = -> { @clock_now }
    @throttle  = Beacon::LogThrottle.new(interval: 60.0, clock: @clock)
  end

  def test_first_call_for_key_emits
    out = capture_stderr do
      @throttle.warn(:k) { |count| "hit (#{count})" }
    end
    assert_match(/\[beacon\] hit \(1\)/, out)
  end

  def test_second_call_within_interval_is_suppressed
    out = capture_stderr do
      @throttle.warn(:k) { |_| "first" }
      @throttle.warn(:k) { |_| "second" }
    end
    assert_match(/first/, out)
    refute_match(/second/, out)
  end

  def test_call_after_interval_emits_and_reports_suppressed_count
    out = capture_stderr do
      @throttle.warn(:k) { |count| "fire (#{count})" }  # count=1
      50.times { @throttle.warn(:k) { |c| "skip (#{c})" } }  # all suppressed
      @clock_now = 61.0
      @throttle.warn(:k) { |count| "fire (#{count})" }  # count=51
    end
    assert_match(/fire \(1\)/, out)
    assert_match(/fire \(51\)/, out)
    refute_match(/skip/, out)
  end

  def test_distinct_keys_are_independent
    out = capture_stderr do
      @throttle.warn(:a) { |_| "A" }
      @throttle.warn(:b) { |_| "B" }
    end
    assert_match(/A/, out)
    assert_match(/B/, out)
  end

  def test_block_is_not_invoked_when_suppressed
    calls = 0
    @throttle.warn(:k) { |_| calls += 1; "first" }
    10.times { @throttle.warn(:k) { |_| calls += 1; "skip" } }
    assert_equal 1, calls, "block must not run on suppressed calls"
  end

  def test_reset_clears_state
    capture_stderr { @throttle.warn(:k) { |_| "first" } }
    @throttle.reset!
    out = capture_stderr { @throttle.warn(:k) { |count| "second (#{count})" } }
    assert_match(/second \(1\)/, out)
  end

  def test_thread_safe_under_contention_preserves_exact_counts
    # 16 threads × 100 warn calls = 1600 total invocations against a
    # key with no prior state. With the clock frozen at t=0, exactly
    # ONE emission should fire (the very first caller to enter the
    # Mutex) and 1599 should be suppressed. Crucially, the suppressed
    # counter must be exactly 1599 — if the Mutex were dropped,
    # concurrent increments would lose writes and land below 1599.
    capture_stderr do
      threads = Array.new(16) do
        Thread.new { 100.times { @throttle.warn(:k) { |_| "x" } } }
      end
      threads.each(&:join)
    end
    state = @throttle.instance_variable_get(:@state)[:k]
    assert_equal 1599, state[:suppressed],
      "expected exactly 1599 suppressed calls under 16x100 contention, got #{state[:suppressed]}"

    # Advance the clock past the interval; the next emission should
    # report count=1600 (1599 suppressed + this one) and reset the
    # suppressed counter to 0.
    @clock_now = 100.0
    msg = nil
    capture_stderr do
      msg = @throttle.warn(:k) { |count| "fire (#{count})" }
    end
    assert_equal "fire (1600)", msg
    assert_equal 0, @throttle.instance_variable_get(:@state)[:k][:suppressed]
  end

  private

  def capture_stderr
    orig = $stderr
    $stderr = StringIO.new
    yield
    $stderr.string
  ensure
    $stderr = orig
  end
end
