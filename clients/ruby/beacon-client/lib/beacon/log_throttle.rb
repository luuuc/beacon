module Beacon
  # Per-key rate limiter for warn/log calls.
  #
  # Without throttling, a misconfigured endpoint or a bad payload can
  # fire the flusher's/subscriber's `warn` branch on every flush or
  # every request — 60 lines/minute at 1 Hz, indefinitely — flooding
  # production.log and burying the signal operators actually need.
  #
  # Usage:
  #   throttle = Beacon::LogThrottle.new(interval: 60.0)
  #   throttle.warn(:flusher_5xx) { |count| "dropped batch (#{count} in the last window)" }
  #
  # The block is only invoked when the key is allowed through, so the
  # message-building cost is paid only on emitted lines. The count
  # argument is the number of suppressed calls since the last emission
  # plus one, so operators can see that "X happened (12 times)" without
  # seeing 12 separate lines.
  #
  # Thread-safe via a single Mutex.
  class LogThrottle
    def initialize(interval: 60.0, clock: -> { Process.clock_gettime(Process::CLOCK_MONOTONIC) })
      @interval = interval
      @clock    = clock
      @mutex    = Mutex.new
      @state    = {}  # key -> { last_at:, suppressed: }
    end

    # Emit a warn line for +key+ if the last emission was more than
    # +interval+ seconds ago (or there was no previous emission).
    # Otherwise increment the suppressed counter and return nil.
    # Yields `count` (Integer, >= 1) so the block can build a message
    # like "X happened (count times in the last minute)".
    def warn(key)
      message = nil
      @mutex.synchronize do
        state = @state[key] ||= { last_at: nil, suppressed: 0 }
        now   = @clock.call
        if state[:last_at].nil? || now - state[:last_at] >= @interval
          count = state[:suppressed] + 1
          state[:last_at]    = now
          state[:suppressed] = 0
          message = yield(count) if block_given?
        else
          state[:suppressed] += 1
        end
      end
      Kernel.warn("[beacon] #{message}") if message
      message
    end

    # Primarily for tests.
    def reset!
      @mutex.synchronize { @state.clear }
    end
  end
end
