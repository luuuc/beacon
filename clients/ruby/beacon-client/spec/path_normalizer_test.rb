require "test_helper"
require "beacon/path_normalizer"

class PathNormalizerTest < Minitest::Test
  include BeaconTestHelper

  def test_conformance_fixtures
    fixtures["path_normalization"]["cases"].each do |c|
      actual = Beacon::PathNormalizer.normalize(c["method"], c["raw_path"])
      assert_equal c["expected_name"], actual,
        "path normalization mismatch for #{c["name"].inspect}"
    end
  end
end
