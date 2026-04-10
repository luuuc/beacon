require "test_helper"
require "beacon/queue"

class QueueTest < Minitest::Test
  def test_push_and_drain
    q = Beacon::Queue.new(max: 100)
    10.times { |i| q.push({ n: i }) }
    assert_equal 10, q.length

    drained = q.drain(5)
    assert_equal 5, drained.length
    assert_equal({ n: 0 }, drained.first)
    assert_equal 5, q.length
  end

  def test_oldest_drop_when_full
    q = Beacon::Queue.new(max: 3)
    5.times { |i| q.push({ n: i }) }
    assert_equal 3, q.length
    assert_equal 2, q.dropped

    remaining = q.drain(10)
    assert_equal [{ n: 2 }, { n: 3 }, { n: 4 }], remaining
  end

  def test_drain_more_than_size
    q = Beacon::Queue.new(max: 100)
    3.times { |i| q.push({ n: i }) }
    assert_equal 3, q.drain(50).length
    assert q.empty?
  end

  def test_thread_safe_push
    q = Beacon::Queue.new(max: 10_000)
    threads = 10.times.map do |t|
      Thread.new do
        100.times { |i| q.push({ t: t, i: i }) }
      end
    end
    threads.each(&:join)
    assert_equal 1_000, q.length
  end

  def test_wait_and_drain_returns_immediately_when_events_present
    q = Beacon::Queue.new(max: 100)
    q.push({ n: 1 })
    start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    events = q.wait_and_drain(10, 60.0)
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - start
    assert_equal 1, events.length
    assert_operator elapsed, :<, 0.1, "should not wait when queue non-empty"
  end

  def test_wait_and_drain_blocks_until_timeout_when_empty
    q = Beacon::Queue.new(max: 100)
    start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    events = q.wait_and_drain(10, 0.05)
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - start
    assert_equal [], events
    assert_operator elapsed, :>=, 0.04
    assert_operator elapsed, :<=, 0.5
  end

  def test_push_signals_waiter_at_flush_threshold
    q = Beacon::Queue.new(max: 100, flush_threshold: 3)
    waiter = Thread.new { q.wait_and_drain(10, 5.0) }
    Thread.pass
    q.push({ n: 1 })
    q.push({ n: 2 })
    # Not yet at threshold (length 2 < 3) — waiter should still be running.
    # Third push crosses the threshold and signals the condvar.
    q.push({ n: 3 })
    events = waiter.join(1).value
    assert_operator events.length, :>=, 1, "waiter should have been signaled"
  end

  def test_burst_from_many_threads_triggers_size_based_flush
    # 10,000 events from 16 threads against flush_threshold=100 must be
    # drained via size-triggered condvar wakes, NOT by the timeout floor.
    # We set the timeout to 10 seconds so the test can only pass if the
    # condvar path actually fires; a broken signal would make the test
    # hit the timeout ceiling 10,000 / 1000 = 10 times (100+ seconds).
    q = Beacon::Queue.new(max: 20_000, flush_threshold: 100)
    drained = []
    drain_mutex = Mutex.new

    consumer = Thread.new do
      total = 0
      loop do
        events = q.wait_and_drain(1_000, 10.0)
        drain_mutex.synchronize { drained.concat(events) }
        total += events.length
        break if total >= 10_000
      end
    end

    start = Process.clock_gettime(Process::CLOCK_MONOTONIC)
    producers = Array.new(16) do |t|
      Thread.new do
        625.times { |i| q.push({ t: t, i: i }) }
      end
    end
    producers.each(&:join)
    consumer.join(5)
    elapsed = Process.clock_gettime(Process::CLOCK_MONOTONIC) - start

    assert_equal 10_000, drained.length,
      "consumer should have drained every event across multiple size-triggered wakes"
    assert_operator elapsed, :<, 1.0,
      "drainage should be size-triggered (sub-second); " \
      "took #{elapsed}s which suggests the condvar wake didn't fire and " \
      "the 10s timeout floor carried the test"
  end

  def test_signal_all_wakes_waiter
    q = Beacon::Queue.new(max: 100, flush_threshold: 100)
    waiter = Thread.new { q.wait_and_drain(10, 60.0) }
    Thread.pass
    q.signal_all
    events = waiter.join(1).value
    assert_equal [], events
  end
end
