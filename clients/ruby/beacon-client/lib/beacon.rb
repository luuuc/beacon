require "beacon/version"
require "beacon/configuration"
require "beacon/fingerprint"
require "beacon/log_throttle"
require "beacon/lru"
require "beacon/path_normalizer"
require "beacon/queue"
require "beacon/transport"
require "beacon/flusher"
require "beacon/client"

module Beacon
  class Error < StandardError; end

  CLIENT_MUTEX = Mutex.new

  class << self
    def configure
      yield config
      warn_if_unusable_endpoint
      reset_client!
      config
    end

    def config
      @config ||= Configuration.new
    end

    def reset_config!
      shutdown
      @config = Configuration.new
    end

    # Thread-safe lazy singleton. Two Puma threads racing on the first
    # request used to be able to create two Clients (each with its own
    # flusher thread) — the second one leaked forever. The Mutex
    # serializes first-access; the double-checked idiom keeps the happy
    # path Mutex-free after initialization.
    def client
      return @client if @client
      CLIENT_MUTEX.synchronize do
        @client ||= Client.new(config: config)
      end
    end

    def reset_client!
      CLIENT_MUTEX.synchronize do
        @client&.shutdown
        @client = nil
      end
    end

    def track(name, properties = {})
      client.track(name, properties)
    end

    # Operator-visible introspection. Returns a flat hash describing
    # the client's current internal state — queue depth, the drop
    # counter, flusher counters, and transport counters. Used by
    # smoke tests and rake tasks; also the primary signal operators
    # have when something gets weird at 3am.
    def stats
      c = @client
      return disabled_stats unless c
      {
        queue_depth:          c.queue.length,
        queue_max:            config.queue_size,
        dropped:              c.queue.dropped,
        sent:                 c.flusher&.stats&.[](:sent) || 0,
        last_flush_at:        c.flusher&.stats&.[](:last_flush_at),
        last_flush_status:    c.flusher&.stats&.[](:last_flush_status),
        circuit_open:         c.flusher&.stats&.[](:circuit_open) || false,
        consecutive_failures: c.flusher&.stats&.[](:consecutive_failures) || 0,
        reconnects:           c.instance_variable_get(:@transport)&.respond_to?(:reconnects) ?
                                c.instance_variable_get(:@transport).reconnects : 0,
        enabled:              c.enabled?,
      }
    end

    def flush
      client.flush
    end

    def shutdown
      CLIENT_MUTEX.synchronize do
        @client&.shutdown
        @client = nil
      end
    end

    private

    def disabled_stats
      {
        queue_depth: 0, queue_max: config.queue_size, dropped: 0,
        sent: 0, last_flush_at: nil, last_flush_status: nil,
        circuit_open: false, consecutive_failures: 0, reconnects: 0,
        enabled: false,
      }
    end

    def warn_if_unusable_endpoint
      return if config.enabled?
      return unless config.enabled  # user explicitly disabled; no warning
      # @enabled is true but endpoint_usable? is false → misconfigured.
      Kernel.warn "[beacon] endpoint is missing or unparseable " \
        "(got #{config.endpoint.inspect}) — running in no-op mode"
    rescue StandardError
      nil
    end
  end
end

require "beacon/rails" if defined?(::Rails::Railtie)

at_exit do
  Beacon.shutdown if Beacon.instance_variable_get(:@client)
end
