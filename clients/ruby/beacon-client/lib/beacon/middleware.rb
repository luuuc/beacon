require "beacon"
require "beacon/lru"

module Beacon
  # Rack middleware that captures perf and errors on the host's hot path.
  #
  # Hot-path discipline (see .doc/definition/05-clients.md):
  #   - capture monotonic start
  #   - call the app
  #   - build a small Hash event
  #   - push to the sink (non-blocking)
  #   - return
  #
  # No JSON encoding, no I/O, no allocation of large buffers. Path
  # normalization is cached per (method, path) so steady-state requests
  # avoid the regex work.
  #
  # The middleware is rescue-all: any exception from Beacon's own code is
  # logged and swallowed. Host exceptions are recorded as errors and then
  # re-raised so the host's normal error handling continues to run.
  class Middleware
    LANGUAGE = "ruby".freeze

    # Upper bound for the stack_trace property. The HTTP API limits any
    # single property value to 16 KB — a 500-frame backtrace easily
    # exceeds that. We keep as many leading frames as fit and append a
    # truncation marker so readers see the trace was clipped.
    STACK_TRACE_MAX_BYTES = 16 * 1024
    STACK_TRACE_TRUNCATED_SUFFIX = "\n… (truncated)".freeze

    # Paths we treat as "not app code" when picking the first app frame.
    # The first pattern catches gem vendor dirs; the second catches Ruby
    # stdlib paths like `/ruby/3.4.0/` or `/ruby-3.4.4/`, without
    # accidentally matching host directories like `clients/ruby/...`
    # that merely contain the word "ruby".
    NON_APP_PATH_PATTERNS = [
      %r{/gems/},
      %r{/ruby[-/]\d},
    ].freeze

    def initialize(app, sink: nil, config: Beacon.config, logger: nil)
      sink ||= Beacon.client
      @app    = app
      @sink   = sink
      @config = config
      @logger = logger
      @app_root = config.app_root.to_s.chomp("/").freeze

      @enabled        = config.enabled?
      @capture_perf   = config.pillar?(:perf)
      @capture_errors = config.pillar?(:errors)
      @enrich_block   = config.enrich_context
      @enrich_warned  = false

      # Pre-built shared context. Frozen so the same Hash is referenced from
      # every event without per-request allocation.
      @base_context = {
        environment: config.environment,
        deploy_sha:  config.deploy_sha,
        language:    LANGUAGE,
      }.compact.freeze

      # Per-(method,path) name cache — bounded LRU. Replaces the old
      # two-level Hash whose eviction check only counted top-level method
      # keys and would have let a bot probing distinct URLs OOM the worker.
      @name_cache = Beacon::LRU.new(max: config.cache_size)

      # Fingerprint -> last full-stack send time (monotonic seconds).
      # Bounded LRU so a misbehaving error class with high-cardinality
      # fingerprints can't grow the throttle map without bound.
      @stack_seen = Beacon::LRU.new(max: config.cache_size)
    end

    # Public stats surface for tests and Beacon.stats. Returns only
    # the counters that are load-bearing for cache-bound debugging:
    # name_cache_size and stack_seen_size. Both are bounded by
    # Configuration#cache_size.
    def stats
      {
        name_cache_size: @name_cache.size,
        stack_seen_size: @stack_seen.size,
      }
    end

    def call(env)
      # Kill-switch fast path: a disabled middleware is a pure passthrough
      # with zero allocations beyond this branch. This is what makes
      # BEACON_DISABLED=1 truly free at request time.
      return @app.call(env) unless @enabled

      start_ns = Process.clock_gettime(Process::CLOCK_MONOTONIC, :nanosecond)
      begin
        status, headers, body = @app.call(env)
      rescue Exception => host_error  # rubocop:disable Lint/RescueException
        capture_perf(env, 500, start_ns) if @capture_perf
        capture_error(env, host_error)   if @capture_errors
        raise
      end
      capture_perf(env, status, start_ns) if @capture_perf
      [status, headers, body]
    end

    private

    def capture_perf(env, status, start_ns)
      now_ns      = Process.clock_gettime(Process::CLOCK_MONOTONIC, :nanosecond)
      duration_ms = (now_ns - start_ns) / 1_000_000
      method      = env["REQUEST_METHOD"] || "GET"
      template    = env["beacon.route_template"]
      path        = env["PATH_INFO"] || "/"
      name        = template || cached_name(method, path)
      dims        = enrich_dimensions(env)

      @sink << {
        kind: :perf,
        name: name,
        created_at_ns: realtime_ns,
        properties: { duration_ms: duration_ms, status: status },
        context: @base_context,
        dimensions: dims,
      }
      nil
    rescue => e
      log_rescue(e)
      nil
    end

    def capture_error(env, exception)
      frame       = first_app_frame(exception)
      fingerprint = Fingerprint.compute(exception.class.name, frame || "")
      send_full   = should_send_full_stack?(fingerprint)
      dims        = enrich_dimensions(env)

      properties = {
        fingerprint:     fingerprint,
        message:         truncate(exception.message.to_s, 500),
        first_app_frame: frame,
      }
      if send_full
        stack = format_stack_trace(exception)
        properties[:stack_trace] = stack if stack
      end

      @sink << {
        kind: :error,
        name: exception.class.name,
        created_at_ns: realtime_ns,
        properties: properties,
        context: @base_context,
        dimensions: dims,
      }
      nil
    rescue => e
      log_rescue(e)
      nil
    end

    # Call the enrichment block safely. Returns a Hash of dimensions or nil.
    # On exception: logs a warning once and returns nil. The block continues
    # to be called on subsequent requests (a transient error like a missing
    # Warden user on a health-check should not permanently disable enrichment).
    # Design invariant: enrichment failures never affect the host app.
    def enrich_dimensions(env)
      return nil unless @enrich_block
      request = Rack::Request.new(env) if defined?(Rack::Request)
      request ||= env
      result = @enrich_block.call(request)
      result.is_a?(Hash) ? result : nil
    rescue => e
      unless @enrich_warned
        log_rescue(e)
        @enrich_warned = true
      end
      nil
    end

    # Hot path. Note: we allocate ONE composite String per request here
    # (`"#{method} #{path}"`). The bench (spec/bench/rack_overhead_bench.rb)
    # shows no measurable regression vs the old two-level Hash, but if
    # Card 9's real-Queue bench ever surfaces one, the recoverable
    # optimization is to nest per-method LRUs and look up via two Hash
    # ops (no String allocation on hit).
    def cached_name(method, path)
      key = "#{method} #{path}"
      @name_cache.compute_if_absent(key) do
        PathNormalizer.normalize(method, path).freeze
      end
    end

    # Check-then-set against the LRU is not atomic across two lock
    # acquisitions, so under heavy contention two threads can both observe
    # `last` as expired for the same fingerprint and both send a full
    # stack. That's acceptable — the dashboard groups events by
    # fingerprint on read, so the worst case is two near-duplicate
    # entries overlaid in the UI. The card's invariant is boundedness,
    # not exact-once throttling.
    def should_send_full_stack?(fingerprint)
      now  = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      last = @stack_seen[fingerprint]
      if last.nil? || now - last > 3600
        @stack_seen[fingerprint] = now
        true
      else
        false
      end
    end

    # Walk backtrace_locations (structured, Ruby-version-stable) rather
    # than string-splitting `backtrace` with `:in `, which silently breaks
    # across Ruby point releases. Returns the first app frame as
    # `relative/path.rb:LINENO`, or nil if no frame belongs to the host
    # app (gem-only stacks, locations missing, etc.).
    def first_app_frame(exception)
      locations = exception.respond_to?(:backtrace_locations) ? exception.backtrace_locations : nil
      return nil unless locations && !locations.empty?

      root_prefix = @app_root + "/"
      locations.each do |loc|
        path = loc.absolute_path || loc.path
        next unless path
        next if NON_APP_PATH_PATTERNS.any? { |p| p.match?(path) }
        next unless path.start_with?(root_prefix)
        rel = path[root_prefix.length..]
        next if rel.start_with?("vendor/")
        return "#{rel}:#{loc.lineno}"
      end
      nil
    end

    # Build a stack_trace property value that fits under
    # STACK_TRACE_MAX_BYTES. Iterates lazily, building each frame's
    # string only when it might still fit, and stops at the budget.
    # This avoids the 500-allocations-to-keep-30 wastage a
    # .map-then-truncate pattern would produce on deep stacks.
    #
    # Uses backtrace_locations when available so the format is stable
    # across Ruby versions. Falls back to exception.backtrace strings
    # otherwise (exotic hosts, rspec mocking, etc.).
    def format_stack_trace(exception)
      locations = exception.respond_to?(:backtrace_locations) ? exception.backtrace_locations : nil
      budget    = STACK_TRACE_MAX_BYTES - STACK_TRACE_TRUNCATED_SUFFIX.bytesize
      kept      = []
      running   = 0
      truncated = false

      each_frame_line(locations, exception.backtrace) do |line|
        size = line.bytesize + 1  # +1 for joining "\n"
        if running + size > budget
          truncated = true
          break
        end
        kept << line
        running += size
      end

      return nil if kept.empty? && !truncated
      return kept.join("\n") unless truncated
      "#{kept.join("\n")}#{STACK_TRACE_TRUNCATED_SUFFIX}"
    end

    def each_frame_line(locations, backtrace)
      if locations && !locations.empty?
        locations.each do |loc|
          yield "#{loc.path}:#{loc.lineno}:in `#{loc.base_label}'"
        end
      elsif backtrace
        backtrace.each { |line| yield line }
      end
    end

    def realtime_ns
      Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond)
    end

    def truncate(str, max)
      str.length > max ? str[0, max] : str
    end

    def log_rescue(error)
      return unless @logger
      @logger.error("[beacon] #{error.class}: #{error.message}")
    rescue
      nil
    end
  end
end
