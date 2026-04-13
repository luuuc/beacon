module Beacon
  # Optional helpers for the `enrich_context` block. These are convenience
  # methods — the block can return any Hash with string keys. None of these
  # are required; an app that knows its users' countries from their profile
  # doesn't need CDN header sniffing.
  module Enrichment
    # CDN geo headers checked in priority order: Cloudflare, Fastly,
    # CloudFront. Returns a two-letter ISO 3166-1 country code or nil.
    CDN_GEO_HEADERS = %w[
      HTTP_CF_IPCOUNTRY
      HTTP_FASTLY_GEO_COUNTRY
      HTTP_CLOUDFRONT_VIEWER_COUNTRY
    ].freeze

    # Returns the two-letter ISO 3166-1 country code from CDN geo headers,
    # or nil when no CDN header is present. Checks Cloudflare, Fastly, and
    # CloudFront in order.
    #
    # Usage inside enrich_context:
    #   c.enrich_context do |request|
    #     { country: Beacon::Enrichment.country_from_cdn(request) }
    #   end
    def self.country_from_cdn(request)
      env = request.respond_to?(:env) ? request.env : request
      CDN_GEO_HEADERS.each do |header|
        value = env[header]
        next if value.nil? || value.empty?
        code = value.strip.upcase
        # "XX" is Cloudflare's "unknown" sentinel; skip it.
        next if code == "XX"
        return code if code.match?(/\A[A-Z]{2}\z/)
      end
      nil
    end
  end
end
