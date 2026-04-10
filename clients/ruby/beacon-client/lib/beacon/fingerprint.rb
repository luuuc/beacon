require "digest/sha1"

module Beacon
  # Fingerprint algorithm — normative, see doc/definition/06-http-api.md.
  #   SHA1("<exception_class>|<first_app_frame_path>")
  # Line numbers are intentionally excluded so cosmetic edits above the
  # failing line don't shatter grouping across deploys.
  module Fingerprint
    LINE_SUFFIX = /:\d+\z/.freeze

    def self.compute(exception_class, first_app_frame)
      path = first_app_frame.to_s.sub(LINE_SUFFIX, "")
      Digest::SHA1.hexdigest("#{exception_class}|#{path}")
    end
  end
end
