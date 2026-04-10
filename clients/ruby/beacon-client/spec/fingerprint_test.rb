require "test_helper"
require "beacon/fingerprint"

class FingerprintTest < Minitest::Test
  include BeaconTestHelper

  def test_conformance_fixtures
    fixtures["fingerprint"]["cases"].each do |c|
      actual = Beacon::Fingerprint.compute(c["exception_class"], c["first_app_frame"])
      assert_equal c["expected_fingerprint"], actual,
        "fingerprint mismatch for #{c["name"].inspect}"
    end
  end

  def test_strips_line_number_from_frame
    with_line    = Beacon::Fingerprint.compute("E", "app/x.rb:42")
    without_line = Beacon::Fingerprint.compute("E", "app/x.rb")
    assert_equal without_line, with_line
  end

  def test_class_change_changes_hash
    refute_equal(
      Beacon::Fingerprint.compute("A", "app/x.rb"),
      Beacon::Fingerprint.compute("B", "app/x.rb"),
    )
  end
end
