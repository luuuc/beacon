require "net/http"
require "uri"

module Beacon
  module Transport
    Result = Struct.new(:status, :retry_after, :error, keyword_init: true) do
      def transport_error?
        !error.nil?
      end
    end

    # Net::HTTP-backed transport. Stdlib only — Beacon's host app should not
    # have to install Faraday/HTTParty just to use the client.
    class Http
      def initialize(config)
        @config = config
        @uri    = URI.parse("#{config.endpoint.to_s.chomp("/")}/events")
      end

      def post(body, idempotency_key:)
        http = Net::HTTP.new(@uri.host, @uri.port)
        http.use_ssl      = (@uri.scheme == "https")
        http.open_timeout = @config.connect_timeout
        http.read_timeout = @config.read_timeout

        req = Net::HTTP::Post.new(@uri.request_uri)
        req["content-type"]    = "application/json"
        req["idempotency-key"] = idempotency_key
        req["authorization"]   = "Bearer #{@config.auth_token}" if @config.auth_token
        req.body = body

        response = http.request(req)
        Result.new(
          status:      response.code.to_i,
          retry_after: response["retry-after"]&.to_i,
          error:       nil,
        )
      rescue => e
        Result.new(status: 0, retry_after: nil, error: e)
      end
    end
  end
end
