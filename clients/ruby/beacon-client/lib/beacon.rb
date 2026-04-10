require "beacon/version"
require "beacon/configuration"
require "beacon/fingerprint"
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
