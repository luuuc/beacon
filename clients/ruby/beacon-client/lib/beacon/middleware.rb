require "beacon"

module Beacon
  # Rack middleware that captures perf and errors on the host's hot path.
  #
  # Hot-path discipline (see doc/definition/05-clients.md):
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

    def initialize(app, sink:, config: Beacon.config, logger: nil)
      @app    = app
      @sink   = sink
      @config = config
      @logger = logger
      @app_root = config.app_root.to_s.chomp("/").freeze

      @capture_perf   = config.pillar?(:perf)
      @capture_errors = config.pillar?(:errors)

      # Pre-built shared context. Frozen so the same Hash is referenced from
      # every event without per-request allocation.
      @base_context = {
        environment: config.environment,
        deploy_sha:  config.deploy_sha,
        language:    LANGUAGE,
      }.compact.freeze

      # Per-(method,path) name cache. Two-level so we don't allocate a
      # composite key string per request.
      @name_cache = {}
      @name_cache_max = 1024

      # Fingerprint -> last full-stack send time (monotonic seconds).
      @stack_seen = {}
      @stack_mutex = Mutex.new
    end

    def call(env)
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
      name        = template ? "#{method} #{template}" : cached_name(method, path)

      @sink << {
        kind: :perf,
        name: name,
        created_at_ns: realtime_ns,
        properties: { duration_ms: duration_ms, status: status },
        context: @base_context,
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

      properties = {
        fingerprint:     fingerprint,
        message:         truncate(exception.message.to_s, 500),
        first_app_frame: frame,
      }
      if send_full && exception.backtrace
        properties[:stack_trace] = exception.backtrace.join("\n")
      end

      @sink << {
        kind: :error,
        name: exception.class.name,
        created_at_ns: realtime_ns,
        properties: properties,
        context: @base_context,
      }
      nil
    rescue => e
      log_rescue(e)
      nil
    end

    def cached_name(method, path)
      by_method = @name_cache[method]
      if by_method
        cached = by_method[path]
        return cached if cached
      else
        by_method = (@name_cache[method] = {})
      end

      computed = PathNormalizer.normalize(method, path).freeze
      if @name_cache.size > @name_cache_max
        @name_cache.clear
        @name_cache[method] = (by_method = {})
      end
      by_method[path] = computed
      computed
    end

    def should_send_full_stack?(fingerprint)
      now = Process.clock_gettime(Process::CLOCK_MONOTONIC)
      @stack_mutex.synchronize do
        last = @stack_seen[fingerprint]
        if last.nil? || now - last > 3600
          @stack_seen[fingerprint] = now
          true
        else
          false
        end
      end
    end

    def first_app_frame(exception)
      bt = exception.backtrace
      return nil unless bt

      root = @app_root
      root_prefix = root + "/"
      bt.each do |line|
        # "/Users/luc/app/app/controllers/x.rb:42:in `index'"
        idx = line.index(":in ")
        path_part = idx ? line[0, idx] : line
        next if path_part.include?("/gems/")
        next if path_part.include?("/ruby/")
        next unless path_part.start_with?(root_prefix)
        rel = path_part[root_prefix.length..]
        next if rel.start_with?("vendor/")
        return rel
      end
      nil
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
