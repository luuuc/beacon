require "test_helper"
require "logger"
require "stringio"
require "rack"
require "rack/mock"
require "beacon/middleware"
require "beacon/enrichment"

class EnrichmentTest < Minitest::Test
  OK_APP = ->(_env) { [200, { "content-type" => "text/plain" }, ["ok"]] }

  def setup
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.environment = "test"
      c.deploy_sha  = "deadbeef"
    end
    @sink = Beacon::Testing::NullSink.new(record: true)
  end

  # -----------------------------------------------------------------------
  # enrich_context block
  # -----------------------------------------------------------------------

  def test_enrichment_flows_to_perf_event_dimensions
    Beacon.configure do |c|
      c.enrich_context do |request|
        { country: "US", plan: "pro" }
      end
    end
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/dashboard", method: "GET")
    mw.call(env)

    event = @sink.events.first
    assert_equal :perf, event[:kind]
    assert_equal({ country: "US", plan: "pro" }, event[:dimensions])
  end

  def test_enrichment_flows_to_error_event_dimensions
    Beacon.configure do |c|
      c.enrich_context do |request|
        { country: "DE" }
      end
    end
    boom_app = ->(_env) { raise RuntimeError, "kaboom" }
    mw = Beacon::Middleware.new(boom_app, sink: @sink)
    env = Rack::MockRequest.env_for("/fail", method: "GET")

    assert_raises(RuntimeError) { mw.call(env) }

    # Both perf and error events should carry dimensions.
    dims = @sink.events.map { |e| e[:dimensions] }
    assert dims.all? { |d| d == { country: "DE" } }, "all events should carry enrichment dimensions"
  end

  def test_no_enrichment_block_dimensions_are_nil
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/", method: "GET")
    mw.call(env)

    event = @sink.events.first
    assert_nil event[:dimensions]
  end

  def test_enrichment_exception_rescued_event_sends_unenriched
    call_count = 0
    Beacon.configure do |c|
      c.enrich_context do |request|
        call_count += 1
        raise "enrichment bug!"
      end
    end
    logger = StringIO.new
    mw = Beacon::Middleware.new(OK_APP, sink: @sink, logger: Logger.new(logger))
    env = Rack::MockRequest.env_for("/", method: "GET")
    mw.call(env)

    # Event still sent, dimensions are nil.
    assert_equal 1, @sink.events.length
    assert_nil @sink.events.first[:dimensions]

    # Warning logged once.
    assert_match(/enrichment bug/, logger.string)

    # Second request: enrichment still called but warning not repeated.
    before_log = logger.string.dup
    env2 = Rack::MockRequest.env_for("/other", method: "GET")
    mw.call(env2)
    assert_equal 2, @sink.events.length
    assert_nil @sink.events.last[:dimensions]
    # call_count proves the block IS still called (it's not permanently disabled).
    assert_equal 2, call_count
    # But the warning was only logged once.
    assert_equal before_log.scan(/enrichment bug/).length, 1
  end

  def test_enrichment_non_hash_return_treated_as_nil
    Beacon.configure do |c|
      c.enrich_context do |request|
        "not a hash"
      end
    end
    mw  = Beacon::Middleware.new(OK_APP, sink: @sink)
    env = Rack::MockRequest.env_for("/", method: "GET")
    mw.call(env)

    assert_nil @sink.events.first[:dimensions]
  end

  # -----------------------------------------------------------------------
  # Beacon::Enrichment.country_from_cdn
  # -----------------------------------------------------------------------

  def test_country_from_cdn_cloudflare
    env = { "HTTP_CF_IPCOUNTRY" => "US" }
    assert_equal "US", Beacon::Enrichment.country_from_cdn(env)
  end

  def test_country_from_cdn_fastly
    env = { "HTTP_FASTLY_GEO_COUNTRY" => "de" }
    assert_equal "DE", Beacon::Enrichment.country_from_cdn(env)
  end

  def test_country_from_cdn_cloudfront
    env = { "HTTP_CLOUDFRONT_VIEWER_COUNTRY" => "JP" }
    assert_equal "JP", Beacon::Enrichment.country_from_cdn(env)
  end

  def test_country_from_cdn_priority_order
    # Cloudflare wins when multiple headers present.
    env = {
      "HTTP_CF_IPCOUNTRY" => "FR",
      "HTTP_FASTLY_GEO_COUNTRY" => "DE",
    }
    assert_equal "FR", Beacon::Enrichment.country_from_cdn(env)
  end

  def test_country_from_cdn_no_header_returns_nil
    assert_nil Beacon::Enrichment.country_from_cdn({})
  end

  def test_country_from_cdn_xx_sentinel_skipped
    # Cloudflare uses "XX" for unknown.
    env = { "HTTP_CF_IPCOUNTRY" => "XX" }
    assert_nil Beacon::Enrichment.country_from_cdn(env)
  end

  def test_country_from_cdn_with_rack_request
    env = Rack::MockRequest.env_for("/", "HTTP_CF_IPCOUNTRY" => "CA")
    request = Rack::Request.new(env)
    assert_equal "CA", Beacon::Enrichment.country_from_cdn(request)
  end
end
