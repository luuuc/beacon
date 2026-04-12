require "net/http"
require "uri"
require "beacon/version"

module Beacon
  module Transport
    Result = Struct.new(:status, :retry_after, :error, keyword_init: true) do
      def transport_error?
        !error.nil?
      end
    end

    # Persistent Net::HTTP-backed transport. Stdlib only — a Beacon host
    # should not need Faraday or HTTParty just to ship events.
    #
    # Why persistent: the Ruby client runs on a host serving real traffic.
    # Opening a fresh TCP+TLS connection per flush (the pre-Card-4 behavior)
    # turns the flusher into a source of background network chatter — one
    # handshake/s sustained on a busy box. That handshake cost shows up in
    # Beacon's own dashboards as noise and adds pointless tail latency on
    # the Beacon server.
    #
    # Reconnect policy: a single persistent connection is held under a
    # Mutex. On `Errno::EPIPE` / `Errno::ECONNRESET` / `EOFError` / `IOError`
    # during `request`, we close the connection, open a fresh one, and
    # retry the request ONCE. If the retry also fails, the exception
    # propagates into `post`'s rescue and surfaces as a `Result` transport
    # error, which the flusher turns into a backoff/retry at its own layer.
    # Two layers of retry is intentional — transport-level heals a flaky
    # socket cheaply, flusher-level handles longer outages with backoff
    # and a circuit breaker.
    #
    # Fork safety: the held connection is a socket FD. If a parent process
    # inherits a forked child, sharing the FD is undefined. `after_fork`
    # drops the connection so the child opens its own on first request.
    # `Client#after_fork` calls it during its own fork handling.
    class Http
      # Errors that mean "this socket is dead, reconnect and retry once."
      # Timeouts are included because a Net::ReadTimeout leaves the
      # Net::HTTP instance in an indeterminate state (partial data
      # buffered, server may still be writing) — continuing to reuse it
      # is a known footgun. Beacon's idempotency-key header makes retry
      # after a timeout safe from the server's perspective.
      RECONNECTABLE_ERRORS = [
        Errno::EPIPE, Errno::ECONNRESET, EOFError, IOError,
        Net::OpenTimeout, Net::ReadTimeout,
      ].freeze

      def initialize(config)
        @config      = config
        @uri         = URI.parse("#{config.endpoint.to_s.chomp("/")}/api/events")
        @mutex       = Mutex.new
        @http        = nil
        @user_agent  = "beacon-client/#{Beacon::VERSION} (ruby #{RUBY_VERSION})".freeze
        @reconnects  = 0
      end

      # Number of times a dead socket was recovered by the reconnect-once
      # path. Exposed via Beacon.stats so operators can distinguish
      # "Beacon P99 spiked because the server was flaky and we healed it"
      # from "Beacon P99 spiked because of something else."
      def reconnects
        @mutex.synchronize { @reconnects }
      end

      def post(body, idempotency_key:)
        response = @mutex.synchronize do
          request_with_reconnect { build_request(body, idempotency_key) }
        end
        Result.new(
          status:      response.code.to_i,
          retry_after: response["retry-after"]&.to_i,
          error:       nil,
        )
      rescue => e
        Result.new(status: 0, retry_after: nil, error: e)
      end

      # Drop the held connection. Called by Client#after_fork so a forked
      # child never writes into a socket FD inherited from its parent.
      def after_fork
        @mutex.synchronize { close_connection }
      end

      private

      def build_request(body, idempotency_key)
        req = Net::HTTP::Post.new(@uri.request_uri)
        req["Content-Type"]    = "application/json"
        req["Idempotency-Key"] = idempotency_key
        req["Authorization"]   = "Bearer #{@config.auth_token}" if @config.auth_token
        req["User-Agent"]      = @user_agent
        req.body = body
        req
      end

      # Runs the block, which must build a fresh Net::HTTP::Post, and
      # sends it. On a reconnectable error, closes the dead socket,
      # re-opens, rebuilds the request via the block (so we never reuse
      # a Net::HTTP::Post that Net::HTTP's internals may have mutated),
      # and retries once. Further failures propagate to post's top-level
      # rescue.
      def request_with_reconnect
        ensure_connected
        begin
          @http.request(yield)
        rescue *RECONNECTABLE_ERRORS
          @reconnects += 1
          close_connection
          ensure_connected
          @http.request(yield)
        end
      end

      def ensure_connected
        return if @http && @http.started?
        @http = Net::HTTP.new(@uri.host, @uri.port)
        @http.use_ssl      = (@uri.scheme == "https")
        @http.open_timeout = @config.connect_timeout
        @http.read_timeout = @config.read_timeout
        @http.start
      end

      def close_connection
        return unless @http
        # The socket may already be dead — finish can raise. Swallow
        # cleanup errors; we're about to drop the reference anyway.
        @http.finish if @http.started?
      rescue
        nil
      ensure
        @http = nil
      end
    end
  end
end
