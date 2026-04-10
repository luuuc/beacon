module Beacon
  # Bounded in-process event queue with oldest-drop semantics.
  #
  # SizedQueue from stdlib is unsuitable here: it blocks producers when full,
  # which would mean Beacon stalls the host's request cycle during a Beacon
  # outage. We need the opposite — drop the oldest event and let the host
  # keep serving.
  #
  # Card 8: the queue also carries the wake-up signal for the flusher. When
  # `length` crosses `flush_threshold` on push, we signal the shared
  # ConditionVariable so the flusher can wake early instead of waiting out
  # the full flush_interval. This turns a 1 Hz polling flusher into an
  # event-driven one for bursty workloads while keeping the periodic tick
  # as the floor for low-traffic apps.
  class Queue
    attr_reader :dropped

    def initialize(max:, flush_threshold: nil)
      @max             = max
      @flush_threshold = flush_threshold
      @mutex           = Mutex.new
      @not_empty       = ConditionVariable.new
      @items           = []
      @dropped         = 0
    end

    def push(event)
      @mutex.synchronize do
        if @items.length >= @max
          @items.shift
          @dropped += 1
        end
        @items << event
        if @flush_threshold && @items.length >= @flush_threshold
          @not_empty.signal
        end
      end
      nil
    end
    alias << push

    def drain(limit)
      @mutex.synchronize { @items.shift(limit) }
    end

    # Block until an event arrives OR `timeout` seconds elapse, then
    # drain up to `limit` events. Used by the flusher's run_loop to
    # wait for either a size-triggered wake-up from Queue#push or the
    # periodic flush_interval floor, whichever comes first.
    def wait_and_drain(limit, timeout)
      @mutex.synchronize do
        @not_empty.wait(@mutex, timeout) if @items.empty?
        @items.shift(limit)
      end
    end

    # Explicit wake-up for the flusher's stop/shutdown path.
    def signal_all
      @mutex.synchronize { @not_empty.broadcast }
    end

    def length
      @mutex.synchronize { @items.length }
    end
    alias size length

    def empty?
      length.zero?
    end
  end
end
