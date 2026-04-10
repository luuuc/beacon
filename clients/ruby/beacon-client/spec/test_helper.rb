$LOAD_PATH.unshift File.expand_path("../lib", __dir__)

require "minitest/autorun"
require "json"
require "beacon"

module BeaconTestHelper
  FIXTURES_PATH = File.expand_path("../../../../spec/fixtures.json", __dir__)

  def fixtures
    @fixtures ||= JSON.parse(File.read(FIXTURES_PATH))
  end
end
