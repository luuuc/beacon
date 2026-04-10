require "minitest/autorun"
require "rbconfig"

# Card 10: `require "beacon"` must NOT load any test-only code. A
# production gem surface that silently pulls NullSink / FakeTransport
# into every Rails boot is a leak of test-only machinery into the
# host app's memory and class namespace.
#
# We verify this by spawning a fresh Ruby process, requiring only
# `beacon`, and asserting `$LOADED_FEATURES` contains no entry for
# `beacon/testing.rb`. The subprocess is necessary because the
# normal test suite loads `beacon/testing` via test_helper long
# before any spec runs.
class TestingIsolationTest < Minitest::Test
  def test_require_beacon_does_not_load_beacon_testing
    lib_path = File.expand_path("../lib", __dir__)
    script = <<~RUBY
      $LOAD_PATH.unshift #{lib_path.inspect}
      require "beacon"
      puts $LOADED_FEATURES.grep(%r{beacon/testing\\.rb\\z}).empty?
    RUBY
    out = IO.popen([RbConfig.ruby, "-e", script], &:read)
    assert_equal "true", out.strip,
      "Expected $LOADED_FEATURES to NOT contain beacon/testing.rb after " \
      "`require 'beacon'` in a fresh process. Got: #{out.inspect}"
  end
end
