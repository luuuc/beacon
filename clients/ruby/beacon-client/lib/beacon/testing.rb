require "beacon"

# Test helpers for beacon-client. **Not loaded by `require "beacon"`.**
# Host tests that need NullSink / FakeTransport / state-reset helpers
# should `require "beacon/testing"` explicitly — keeping the production
# gem surface free of test-only code.
#
# Contents:
#   - Beacon::Testing::NullSink       — sink that drops or records events
#   - Beacon::Testing::FakeTransport  — transport that captures batches
#                                        and lets tests script responses
#   - Beacon::Testing.reset_config!   — drop the memoized Configuration
#   - Beacon::Testing.reset_client!   — shutdown and clear Beacon.client
module Beacon
  module Testing
    # Sink that drops or records events. Used by the Rack overhead bench
    # (drop mode) and by middleware tests (record mode).
    class NullSink
      attr_reader :events

      def initialize(record: false)
        @record = record
        @events = record ? [] : nil
      end

      def push(event)
        @events << event if @record
        nil
      end
      alias << push

      def length
        @record ? @events.length : 0
      end
    end

    # Test transport that records every batch and lets the test decide
    # what response to return next. No real HTTP, no sockets.
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

    # Drop the memoized Configuration and any associated Client. Use
    # in test setup so each test gets a fresh Configuration.
    #
    # Reaches into Beacon's internals via instance_variable_set
    # because we deliberately removed `Beacon.reset_config!` from the
    # production surface (Card 10) — test-only machinery should not
    # live on the main module. If you're tempted to "clean this up"
    # by adding `reset_config!` back to Beacon, read the pitch first.
    def self.reset_config!
      Beacon.shutdown
      Beacon.instance_variable_set(:@config, nil)
    end

    # Alias for Beacon.shutdown, kept as a separate name because
    # `reset_client!` reads correctly at call sites that are
    # logically "replace the client without changing config" —
    # Beacon.shutdown at those sites would be semantically correct
    # but misleading (it sounds like tearing down the whole gem).
    def self.reset_client!
      Beacon.shutdown
    end
  end
end
