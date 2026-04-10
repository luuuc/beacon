module Beacon
  module Test
    # Test transport that records every batch and lets the test decide what
    # response to return next. No real HTTP, no sockets.
    class FakeTransport
      attr_reader :batches

      def initialize
        @batches  = []
        @mutex    = Mutex.new
        @next     = []  # queued Beacon::Transport::Result objects
      end

      def post(body, idempotency_key:)
        @mutex.synchronize do
          @batches << { body: body, idempotency_key: idempotency_key }
          if (planned = @next.shift)
            return planned
          end
        end
        Beacon::Transport::Result.new(status: 202, retry_after: nil, error: nil)
      end

      # Queue a planned result for the next post call.
      def respond_with(status: 202, retry_after: nil, error: nil)
        @mutex.synchronize do
          @next << Beacon::Transport::Result.new(
            status: status, retry_after: retry_after, error: error,
          )
        end
      end

      def reset!
        @mutex.synchronize do
          @batches.clear
          @next.clear
        end
      end
    end
  end
end
