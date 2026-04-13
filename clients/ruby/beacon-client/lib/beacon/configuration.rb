require "uri"

module Beacon
  class Configuration
    attr_accessor :endpoint, :environment, :deploy_sha, :auth_token,
                  :async, :app_root, :pillars,
                  :flush_interval, :flush_threshold, :queue_size,
                  :connect_timeout, :read_timeout,
                  :cache_size, :enabled, :ambient

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

      # Ambient mode: when true, middleware sends kind: 'ambient' events
      # for HTTP requests in addition to perf events.
      @ambient = false

      # Enrichment block: called on every request to provide dimensions
      # (country, plan, locale, etc.) that flow to all event kinds.
      @enrich_context_block = nil

      # Global kill switch. When false, Beacon::Middleware is a
      # passthrough, Beacon.track returns nil, and the flusher thread
      # is not started.
      #
      # Default resolution (in priority order):
      #   1. BEACON_DISABLED explicitly set → honored in both directions.
      #        "1" / "true" / "yes" / "on"   → disabled
      #        "0" / "false" / "no" / "off"  → forced enabled
      #   2. RAILS_ENV / RACK_ENV is "test" → disabled.
      #   3. Otherwise → enabled (development, staging, production).
      #
      # The test-env default matches Honeybadger/Sentry/AppSignal: an
      # observability gem should not chatter across a hermetic test
      # suite by default. A test that WANTS to assert Beacon was called
      # (via Beacon::Testing::FakeTransport) opts back in locally, or
      # sets BEACON_DISABLED=0 for the whole run.
      @enabled = default_enabled
    end

    def enabled?
      @enabled && endpoint_usable?
    end

    def pillar?(name)
      @pillars.include?(name)
    end

    # Register or read the enrichment block. With a block: registers it.
    # Without: returns the current block (or nil). The block receives a
    # Rack request and returns a Hash of dimensions (e.g. { country: "US" }).
    def enrich_context(&block)
      if block
        @enrich_context_block = block
      else
        @enrich_context_block
      end
    end

    private

    def default_enabled
      v = ENV["BEACON_DISABLED"]
      return !truthy_env?("BEACON_DISABLED") unless v.nil? || v.empty?
      !test_environment?
    end

    def test_environment?
      ENV["RAILS_ENV"] == "test" || ENV["RACK_ENV"] == "test"
    end

    def truthy_env?(name)
      v = ENV[name]
      return false if v.nil? || v.empty?
      !%w[0 false no off].include?(v.downcase)
    end

    def endpoint_usable?
      return false if endpoint.nil? || endpoint.to_s.empty?
      URI.parse(endpoint.to_s)
      true
    rescue URI::InvalidURIError
      false
    end
  end
end
