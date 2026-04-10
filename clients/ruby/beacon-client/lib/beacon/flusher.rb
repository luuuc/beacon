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
        send_batch(events)
      end
    rescue => e
      log_rescue(e)
    end

    private

    def run_loop
      until @stop
        sleep @config.flush_interval
        next if circuit_open?
        events = @client.queue.drain(DRAIN_BATCH)
        send_batch(events) unless events.empty?
      end
    rescue => e
      log_rescue(e)
    end

    def send_batch(events)
      payload         = serialize(events)
      idempotency_key = SecureRandom.uuid

      if post_with_retries(payload, idempotency_key)
        record_flush(events.length, :ok)
        @consecutive_failures = 0
      else
        record_flush(0, :failed)
        @consecutive_failures += 1
        if @consecutive_failures >= CIRCUIT_OPEN_THRESHOLD
          @circuit_open_until = monotonic + CIRCUIT_OPEN_SECONDS
          @log_throttle.warn(:circuit_open) do |count|
            suffix = count > 1 ? " (#{count} times in the last minute)" : ""
            "circuit breaker opened — pausing flushes for #{CIRCUIT_OPEN_SECONDS}s#{suffix}"
          end
        end
      end
    rescue => e
      log_rescue(e)
    end

    def record_flush(sent_count, status)
      @stats_mutex.synchronize do
        @sent             += sent_count
        @last_flush_at     = Time.now.utc
        @last_flush_status = status
      end
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

    def serialize(events)
      JSON.generate(events: events.map { |e| serialize_event(e) })
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
      return false unless @circuit_open_until
      if monotonic >= @circuit_open_until
        @circuit_open_until   = nil
        @consecutive_failures = 0
        return false
      end
      true
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
