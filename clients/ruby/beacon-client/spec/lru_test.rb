require "test_helper"
require "beacon/lru"

class LRUTest < Minitest::Test
  def test_rejects_nonpositive_max
    assert_raises(ArgumentError) { Beacon::LRU.new(max: 0) }
    assert_raises(ArgumentError) { Beacon::LRU.new(max: -1) }
    assert_raises(ArgumentError) { Beacon::LRU.new(max: 1.5) }
  end

  def test_read_on_miss_returns_nil
    lru = Beacon::LRU.new(max: 4)
    assert_nil lru["absent"]
  end

  def test_write_stores_and_returns_value
    lru = Beacon::LRU.new(max: 4)
    assert_equal "v", (lru["k"] = "v")
    assert_equal "v", lru["k"]
  end

  def test_write_overwrites_existing_key_without_growing
    lru = Beacon::LRU.new(max: 2)
    lru["a"] = 1
    lru["a"] = 2
    assert_equal 2, lru["a"]
    assert_equal 1, lru.size
  end

  def test_overflow_evicts_oldest_entry
    lru = Beacon::LRU.new(max: 2)
    lru["a"] = 1
    lru["b"] = 2
    lru["c"] = 3
    assert_nil lru["a"],        "a should have been evicted as oldest"
    assert_equal 2, lru["b"]
    assert_equal 3, lru["c"]
    assert_equal 2, lru.size
  end

  def test_read_moves_key_to_tail_preventing_eviction
    lru = Beacon::LRU.new(max: 2)
    lru["a"] = 1
    lru["b"] = 2
    lru["a"]           # "a" is now the MRU
    lru["c"] = 3       # should evict "b", not "a"
    assert_equal 1, lru["a"]
    assert_nil     lru["b"]
    assert_equal 3, lru["c"]
  end

  def test_compute_if_absent_computes_on_miss_and_memoizes
    lru = Beacon::LRU.new(max: 4)
    calls = 0
    first  = lru.compute_if_absent("k") { calls += 1; "computed" }
    second = lru.compute_if_absent("k") { calls += 1; "again" }
    assert_equal "computed", first
    assert_equal "computed", second
    assert_equal 1, calls
  end

  def test_compute_if_absent_block_runs_outside_the_mutex
    # Pin the documented semantics: two concurrent misses on the same
    # key both run the block. The second []= overwrites the first. If
    # a future refactor adds a per-key compute lock, update this test.
    lru = Beacon::LRU.new(max: 4)
    start_barrier = Mutex.new
    in_block      = ConditionVariable.new
    proceed       = ConditionVariable.new
    waiting       = 0
    calls         = 0

    workers = Array.new(2) do
      Thread.new do
        lru.compute_if_absent("k") do
          start_barrier.synchronize do
            calls   += 1
            waiting += 1
            in_block.signal
            proceed.wait(start_barrier)
          end
          "value"
        end
      end
    end

    start_barrier.synchronize do
      in_block.wait(start_barrier) while waiting < 2
      proceed.broadcast
    end
    workers.each(&:join)
    assert_equal 2, calls, "both racing threads should have run the block"
    assert_equal "value", lru["k"]
  end

  def test_size_is_capped_under_large_insert_volume
    lru = Beacon::LRU.new(max: 1024)
    10_000.times { |i| lru["k#{i}"] = i }
    assert_equal 1024, lru.size
  end

  def test_concurrent_inserts_do_not_corrupt_state
    lru = Beacon::LRU.new(max: 1024)
    threads = 16
    per_thread = 1000
    ts = Array.new(threads) do |t|
      Thread.new do
        per_thread.times do |i|
          key = "t#{t}-k#{i}"
          lru[key] = i
          lru[key]  # may or may not hit, but must not crash
        end
      end
    end
    ts.each(&:join)
    assert_operator lru.size, :<=, 1024
  end

  def test_clear_empties_the_cache
    lru = Beacon::LRU.new(max: 4)
    lru["a"] = 1; lru["b"] = 2
    lru.clear
    assert_equal 0, lru.size
    assert_nil lru["a"]
  end
end
