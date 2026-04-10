require "json"
require "securerandom"

module Beacon
  # Background flusher: drains the bounded queue, builds a JSON batch, and
  # POSTs it through the transport. Implements the retry, circuit breaker,
  # and Idempotency-Key behavior from doc/definition/05-clients.md.
  #
  # The flusher is rescue-all. Its loop crashing is itself an event we log
  # but never re-raise — the host's request cycle keeps running.
  class Flusher
    BACKOFF_SECONDS         = [0.2, 0.4, 0.8, 1.6, 3.2].freeze
    CIRCUIT_OPEN_SECONDS    = 30.0
    CIRCUIT_OPEN_THRESHOLD  = 5
    DRAIN_BATCH             = 1_000
    SHUTDOWN_FLUSH_TIMEOUT  = 2.0
    # Split serialized batches at this byte count. The Beacon server's
    # /events endpoint limits any single request body to roughly 1 MB;
    # 800 KB is a conservative ceiling that leaves headroom for headers
    # and JSON framing. When a single flush produces more, we issue
    # multiple POSTs rather than failing with 413.
    BATCH_MAX_BYTES         = 800 * 1024

    def initialize(client, transport:, backoff: BACKOFF_SECONDS, log_throttle: nil)
      @client    = client
      @config    = client.config
      @transport = transport
      @backoff   = backoff
      @log_throttle = log_throttle || Beacon::LogThrottle.new

      @stop                 = false
      @thread               = nil
      @consecutive_failures = 0
      @circuit_open_until   = nil

      # Observability counters — read by Beacon.stats. Events written
      # under @stats_mutex so concurrent reads (from Beacon.stats on
      # the main thread) see a consistent snapshot.
      @stats_mutex       = Mutex.new
      @sent              = 0
      @last_flush_at     = nil
      @last_flush_status = nil
    end

    def stats
      @stats_mutex.synchronize do
        {
          sent:                 @sent,
          last_flush_at:        @last_flush_at,
          last_flush_status:    @last_flush_status,
          consecutive_failures: @consecutive_failures,
          circuit_open:         !@circuit_open_until.nil?,
        }
      end
    end

    def start
      @stop   = false
      @thread = Thread.new { run_loop }
      @thread.name = "beacon-flusher" if @thread.respond_to?(:name=)
      @thread
    end

    def stop
      @stop = true
      @client.queue.signal_all  # wake wait_and_drain from its condvar
      @thread&.join(SHUTDOWN_FLUSH_TIMEOUT)
      flush_now
      nil
    end

    def alive?
      @thread&.alive? ? true : false
    end

    def flush_now
      loop do
        events = @client.queue.drain(DRAIN_BATCH)
        break if events.empty?
        send_events(events)
      end
    rescue => e
      log_rescue(e)
    end

    private

    def run_loop
      until @stop
        # Card 8: wait_and_drain blocks on a ConditionVariable with
        # the flush_interval as the timeout ceiling. Queue#push signals
        # the condvar when size crosses flush_threshold, so bursty
        # workloads flush early instead of waiting out the full
        # interval. Low-traffic apps still flush every interval as a
        # floor.
        events = @client.queue.wait_and_drain(DRAIN_BATCH, @config.flush_interval)
        break if @stop
        next if circuit_open?
        send_events(events) unless events.empty?
      end
    rescue => e
      log_rescue(e)
    end

    # Split one drain's worth of events into body-size-bounded POSTs.
    # A single flush call may produce multiple send_batch calls when
    # the serialized payload exceeds BATCH_MAX_BYTES — we do NOT drop
    # events, we issue more POSTs. Serialize each event once, then
    # walk the list accumulating a bytes budget.
    def send_events(events)
      serialized = events.map { |e| serialize_event(e) }
      sizes      = serialized.map { |h| JSON.generate(h).bytesize }

      batch = []
      running_bytes = 0
      envelope_overhead = JSON.generate(events: []).bytesize  # "{\"events\":[]}" = 13 bytes
      per_item_overhead = 1  # comma separator between items
      serialized.each_with_index do |event, i|
        size = sizes[i] + per_item_overhead
        if batch.any? && running_bytes + size + envelope_overhead > BATCH_MAX_BYTES
          send_batch(batch)
          batch = []
          running_bytes = 0
        end
        batch << event
        running_bytes += size
      end
      send_batch(batch) unless batch.empty?
    end

    def send_batch(events)
      payload         = JSON.generate(events: events)
      idempotency_key = SecureRandom.uuid
      ok              = post_with_retries(payload, idempotency_key)

      # All counter writes go through @stats_mutex so the Beacon.stats
      # reader on the main thread sees a consistent snapshot under
      # non-MRI Rubies. MRI's GVL would make unlocked writes "work" by
      # accident; TruffleRuby / JRuby would race.
      opened_circuit = false
      @stats_mutex.synchronize do
        if ok
          @sent                 += events.length
          @last_flush_at         = Time.now.utc
          @last_flush_status     = :ok
          @consecutive_failures  = 0
        else
          @last_flush_at         = Time.now.utc
          @last_flush_status     = :failed
          @consecutive_failures += 1
          if @consecutive_failures >= CIRCUIT_OPEN_THRESHOLD && @circuit_open_until.nil?
            @circuit_open_until = monotonic + CIRCUIT_OPEN_SECONDS
            opened_circuit = true
          end
        end
      end

      if opened_circuit
        @log_throttle.warn(:circuit_open) do |count|
          suffix = count > 1 ? " (#{count} times in the last minute)" : ""
          "circuit breaker opened — pausing flushes for #{CIRCUIT_OPEN_SECONDS}s#{suffix}"
        end
      end
    rescue => e
      log_rescue(e)
    end

    def post_with_retries(payload, idempotency_key)
      @backoff.each_with_index do |sleep_for, i|
        result = @transport.post(payload, idempotency_key: idempotency_key)

        if result.transport_error?
          # fall through to backoff + retry
        elsif result.status == 200 || result.status == 202
          return true
        elsif result.status == 400 || result.status == 401 || result.status == 413
          @log_throttle.warn(:"drop_#{result.status}") do |count|
            suffix = count > 1 ? " (#{count} in the last minute)" : ""
            "dropping batch — server returned #{result.status}#{suffix}"
          end
          return false
        elsif result.status == 429
          sleep(result.retry_after && result.retry_after > 0 ? result.retry_after : 1)
          next
        elsif result.status >= 500 && result.status < 600
          # retry
        else
          @log_throttle.warn(:"drop_#{result.status}") do |count|
            suffix = count > 1 ? " (#{count} in the last minute)" : ""
            "unexpected status #{result.status} — dropping batch#{suffix}"
          end
          return false
        end

        sleep(sleep_for) unless i == @backoff.length - 1
      end
      false
    end

    def serialize_event(event)
      out = {
        kind:       event[:kind].to_s,
        name:       event[:name],
        created_at: format_iso8601(event[:created_at_ns]),
      }
      out[:actor_type] = event[:actor_type] if event[:actor_type]
      out[:actor_id]   = event[:actor_id]   if event[:actor_id]
      props = event[:properties]
      out[:properties] = props if props && !props.empty?
      ctx = event[:context]
      out[:context] = ctx if ctx && !ctx.empty?
      out
    end

    def format_iso8601(ns)
      return Time.now.utc.iso8601(6) if ns.nil?
      sec  = ns / 1_000_000_000
      nsec = ns % 1_000_000_000
      Time.at(sec, nsec, :nanosecond).utc.strftime("%Y-%m-%dT%H:%M:%S.%6NZ")
    end

    def circuit_open?
      @stats_mutex.synchronize do
        return false unless @circuit_open_until
        if monotonic >= @circuit_open_until
          @circuit_open_until   = nil
          @consecutive_failures = 0
          return false
        end
        true
      end
    end

    def monotonic
      Process.clock_gettime(Process::CLOCK_MONOTONIC)
    end

    def log_rescue(error)
      @log_throttle.warn(:"rescue_#{error.class.name}") do |count|
        suffix = count > 1 ? " (#{count} in the last minute)" : ""
        "flusher rescued #{error.class}: #{error.message}#{suffix}"
      end
    end
  end
end
