module Beacon
  # Path normalization fallback — normative, see doc/definition/06-http-api.md.
  #
  # Used when the host framework does not expose a route template. Rails apps
  # should set env["beacon.route_template"] from the routes layer; everything
  # else falls back to this heuristic.
  module PathNormalizer
    NUMERIC = /\A\d+\z/.freeze
    UUID    = /\A[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\z/.freeze
    TOKEN   = /\A[A-Za-z0-9_\-]{22,}\z/.freeze

    ID  = ":id".freeze
    UID = ":uuid".freeze
    TOK = ":token".freeze

    def self.normalize(method, raw_path)
      path = raw_path.to_s
      qs = path.index("?")
      path = path[0, qs] if qs

      segments = path.split("/", -1)
      segments.map! do |seg|
        if seg.empty?
          seg
        elsif seg.match?(NUMERIC)
          ID
        elsif seg.match?(UUID)
          UID
        elsif seg.match?(TOKEN)
          TOK
        else
          seg
        end
      end

      "#{method.to_s.upcase} #{segments.join("/")}"
    end
  end
end
