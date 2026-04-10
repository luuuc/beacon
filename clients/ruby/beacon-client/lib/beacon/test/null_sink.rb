module Beacon
  module Test
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
  end
end
