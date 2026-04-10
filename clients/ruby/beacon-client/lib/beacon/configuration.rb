module Beacon
  class Configuration
    attr_accessor :endpoint, :environment, :deploy_sha, :auth_token,
                  :async, :app_root, :pillars,
                  :flush_interval, :flush_threshold, :queue_size,
                  :connect_timeout, :read_timeout,
                  :cache_size

    def initialize
      @endpoint        = ENV["BEACON_ENDPOINT"] || "http://127.0.0.1:4680"
      @environment     = ENV["BEACON_ENVIRONMENT"] || ENV["RAILS_ENV"] || "development"
      @deploy_sha      = ENV["GIT_SHA"] || ENV["KAMAL_VERSION"]
      @auth_token      = ENV["BEACON_AUTH_TOKEN"]
      @async           = true
      @app_root        = Dir.pwd
      @pillars         = %i[outcomes perf errors]
      @flush_interval  = 1.0
      @flush_threshold = 100
      @queue_size      = 10_000
      @connect_timeout = 1.0
      @read_timeout    = 2.0
      # Shared cap for the middleware's LRU caches (per-request path
      # name cache and per-fingerprint stack-throttle cache). One knob
      # because both caches sit on the same Middleware instance, both
      # are bounded for the same reason (protect against high-cardinality
      # probes), and there is no realistic scenario where one should be
      # tuned independently of the other.
      @cache_size = 1024
    end

    def pillar?(name)
      @pillars.include?(name)
    end
  end
end
