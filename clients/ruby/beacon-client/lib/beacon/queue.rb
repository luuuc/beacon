module Beacon
  # Bounded in-process event queue with oldest-drop semantics.
  #
  # SizedQueue from stdlib is unsuitable here: it blocks producers when full,
  # which would mean Beacon stalls the host's request cycle during a Beacon
  # outage. We need the opposite — drop the oldest event and let the host
  # keep serving.
  class Queue
    attr_reader :dropped

    def initialize(max:)
      @max     = max
      @mutex   = Mutex.new
      @items   = []
      @dropped = 0
    end

    def push(event)
      @mutex.synchronize do
        if @items.length >= @max
          @items.shift
          @dropped += 1
        end
        @items << event
      end
      nil
    end
    alias << push

    def drain(limit)
      @mutex.synchronize { @items.shift(limit) }
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
