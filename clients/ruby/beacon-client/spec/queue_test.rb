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
end
