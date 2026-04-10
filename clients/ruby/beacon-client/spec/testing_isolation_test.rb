require "minitest/autorun"
require "json"

# Card 10: `require "beacon"` must NOT load any test-only code. A
# production gem surface that silently pulls NullSink / FakeTransport
# into every Rails boot is a leak of test-only machinery into the
# host app's memory and class namespace.
#
# This test runs in a subprocess so its `require "beacon"` sees a
# fresh VM with no previously-loaded `beacon/testing`. That's the
# only honest way to prove the contract — the other spec files
# require "beacon/testing" via test_helper, so by the time any
# normal test runs, the helpers are already loaded.
class TestingIsolationTest < Minitest::Test
  def test_require_beacon_does_not_load_beacon_testing
    lib_path = File.expand_path("../lib", __dir__)
    script = <<~RUBY
      $LOAD_PATH.unshift #{lib_path.inspect}
      require "beacon"
      result = {
        testing_defined:      defined?(Beacon::Testing) ? true : false,
        null_sink_defined:    defined?(Beacon::Testing::NullSink) ? true : false,
        fake_transport_defined: defined?(Beacon::Testing::FakeTransport) ? true : false,
        reset_config_on_main:   Beacon.respond_to?(:reset_config!),
        reset_client_on_main:   Beacon.respond_to?(:reset_client!),
      }
      require "json"
      puts JSON.generate(result)
    RUBY
    out = IO.popen([RbConfig.ruby, "-e", script], &:read)
    result = JSON.parse(out)
    refute result["testing_defined"],        "Beacon::Testing leaked into `require 'beacon'`"
    refute result["null_sink_defined"],      "NullSink leaked into `require 'beacon'`"
    refute result["fake_transport_defined"], "FakeTransport leaked into `require 'beacon'`"
    refute result["reset_config_on_main"],   "Beacon.reset_config! should only live in Beacon::Testing"
    refute result["reset_client_on_main"],   "Beacon.reset_client! should only live in Beacon::Testing"
  end

  def test_require_beacon_testing_defines_the_helpers
    lib_path = File.expand_path("../lib", __dir__)
    script = <<~RUBY
      $LOAD_PATH.unshift #{lib_path.inspect}
      require "beacon"
      require "beacon/testing"
      result = {
        testing_defined:        defined?(Beacon::Testing) ? true : false,
        null_sink_defined:      defined?(Beacon::Testing::NullSink) ? true : false,
        fake_transport_defined: defined?(Beacon::Testing::FakeTransport) ? true : false,
        reset_config_in_testing: Beacon::Testing.respond_to?(:reset_config!),
        reset_client_in_testing: Beacon::Testing.respond_to?(:reset_client!),
      }
      require "json"
      puts JSON.generate(result)
    RUBY
    out = IO.popen([RbConfig.ruby, "-e", script], &:read)
    result = JSON.parse(out)
    assert result["testing_defined"]
    assert result["null_sink_defined"]
    assert result["fake_transport_defined"]
    assert result["reset_config_in_testing"]
    assert result["reset_client_in_testing"]
  end
end
