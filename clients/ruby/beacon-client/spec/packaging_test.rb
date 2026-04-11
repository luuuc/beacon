require "test_helper"

# Regression guard for the "beacon-client gem loads, but Beacon.track is
# undefined" bug found during the Maket integration.
#
# The bug: Bundler auto-requires the file whose name matches the gem's
# name. For `beacon-client` it tries `require "beacon-client"` first,
# then `require "beacon/client"`. The second path happened to work
# because `lib/beacon/client.rb` exists — but it only loads the Client
# class and its direct deps, NOT `lib/beacon.rb`, which is the file
# that defines `Beacon.track`, `Beacon.configure`, and loads the Railtie.
#
# With `lib/beacon-client.rb` as a shim (`require "beacon"`), Bundler's
# default require now lands on the real entry point and the full
# module API becomes available.
#
# These tests run in a FRESH Ruby process so the result isn't
# contaminated by the `require "beacon"` call in test_helper.rb.
class PackagingTest < Minitest::Test
  GEM_LIB = File.expand_path("../lib", __dir__).freeze

  def test_require_beacon_client_exposes_module_api
    out, status = run_in_fresh_ruby(<<~RUBY)
      require "beacon-client"
      missing = %i[track configure flush stats shutdown].reject { |m| Beacon.respond_to?(m) }
      if missing.any?
        warn "MISSING: " + missing.inspect
        exit 1
      end
      print "ok"
    RUBY

    assert status.success?,
      "fresh `require \"beacon-client\"` failed to expose Beacon module API. stdout+stderr: #{out}"
    assert_equal "ok", out.strip
  end

  def test_require_beacon_slash_client_also_chains_through
    # `require "beacon/client"` is the other form Bundler sometimes
    # tries. It historically loaded ONLY the Client class — no module
    # API. This test pins that the shim-less path still works too, so
    # we don't regress the second autoresolution route.
    out, status = run_in_fresh_ruby(<<~RUBY)
      require "beacon/client"
      # beacon/client.rb does not itself require "beacon", so this
      # intentionally still lacks the module API. Assert the boundary:
      # Beacon::Client is defined, Beacon.track is NOT.
      unless defined?(Beacon::Client)
        warn "MISSING: Beacon::Client constant"
        exit 1
      end
      if Beacon.respond_to?(:track)
        warn "UNEXPECTED: Beacon.track is defined via require \\"beacon/client\\""
        exit 1
      end
      print "ok"
    RUBY

    assert status.success?,
      "require \"beacon/client\" boundary regressed. stdout+stderr: #{out}"
    assert_equal "ok", out.strip
  end

  private

  def run_in_fresh_ruby(code)
    require "open3"
    Open3.capture2e(RbConfig.ruby, "-I", GEM_LIB, "-e", code)
  end
end
