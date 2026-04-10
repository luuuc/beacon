module Beacon
  # Bounded LRU cache, Mutex-guarded.
  #
  # Exploits the fact that Ruby's Hash preserves insertion order: on a read
  # hit we delete + re-insert (moves the key to the tail), and on overflow
  # we shift the oldest entry. No linked list, no external dependency.
  #
  # The critical section is intentionally minimal — [], []=, and
  # compute_if_absent all run a handful of Hash operations under the
  # Mutex. The middleware's per-request path cache lives behind this
  # class, so the overhead is on the hot path.
  #
  # Footgun: this LRU does NOT distinguish a stored nil from a miss.
  # `lru[key]` returns nil in both cases. Callers that need to cache
  # nil should store a sentinel (e.g. :__missing__) instead.
  class LRU
    def initialize(max:)
      raise ArgumentError, "max must be positive" unless max.is_a?(Integer) && max > 0
      @max   = max
      @store = {}
      @mutex = Mutex.new
    end

    def [](key)
      @mutex.synchronize do
        return nil unless @store.key?(key)
        value = @store.delete(key)
        @store[key] = value
        value
      end
    end

    def []=(key, value)
      @mutex.synchronize do
        @store.delete(key) if @store.key?(key)
        @store[key] = value
        # A single []= can grow the store by at most one entry, so an
        # `if` is sufficient — no need for `while`.
        @store.shift if @store.size > @max
        value
      end
    end

    # Read-or-compute primitive. On hit, returns the cached value. On
    # miss, yields the key, stores the result, and returns it.
    #
    # The compute block runs OUTSIDE the mutex. Two threads concurrently
    # missing on the same key will both run the block — the second
    # []= then overwrites the first. Use this only when the block is
    # idempotent and cheap (e.g. pure transforms like PathNormalizer).
    # Callers that need exactly-once compute under contention should
    # build their own guard around this class.
    def compute_if_absent(key)
      hit = self[key]
      return hit unless hit.nil?
      self[key] = yield(key)
    end

    def size
      @mutex.synchronize { @store.size }
    end

    def clear
      @mutex.synchronize { @store.clear }
    end
  end
end
