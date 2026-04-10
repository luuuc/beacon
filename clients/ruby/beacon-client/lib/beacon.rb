require "beacon/version"
require "beacon/configuration"
require "beacon/fingerprint"
require "beacon/path_normalizer"
require "beacon/queue"
require "beacon/transport"
require "beacon/flusher"
require "beacon/client"

module Beacon
  class Error < StandardError; end

  class << self
    def configure
      yield config
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

    def client
      @client ||= Client.new(config: config)
    end

    def reset_client!
      @client&.shutdown
      @client = nil
    end

    def track(name, properties = {})
      client.track(name, properties)
    end

    def flush
      client.flush
    end

    def shutdown
      @client&.shutdown
      @client = nil
    end
  end
end

require "beacon/rails" if defined?(::Rails::Railtie)

at_exit do
  Beacon.shutdown if Beacon.instance_variable_get(:@client)
end
