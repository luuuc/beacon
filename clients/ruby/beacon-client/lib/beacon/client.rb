require "beacon/queue"
require "beacon/flusher"
require "beacon/transport"

module Beacon
  # The top-level client. Owns the queue and the flusher, exposes track,
  # implements fork safety. Beacon.track / Beacon.flush / Beacon.shutdown
  # all delegate here.
  class Client
    LANGUAGE = "ruby".freeze

    attr_reader :config, :queue, :flusher

    def initialize(config:, transport: nil, autostart: true)
      @config    = config
      @transport = transport || Transport::Http.new(config)
      @queue     = Beacon::Queue.new(max: config.queue_size)
      @pid       = Process.pid
      @mutex     = Mutex.new
      start_flusher if autostart && config.async
    end

    # Outcomes API. The :user shorthand is the only magic — everything else
    # in the properties hash flows through unchanged.
    def track(name, properties = {})
      return nil unless @config.pillar?(:outcomes)
      props      = properties.dup
      actor_type, actor_id = extract_actor(props)

      push({
        kind:          :outcome,
        name:          name.to_s,
        created_at_ns: realtime_ns,
        actor_type:    actor_type,
        actor_id:      actor_id,
        properties:    props,
        context:       base_context,
      })
    rescue => e
      warn "[beacon] track failed: #{e.class}: #{e.message}"
      nil
    end

    # Sink interface — what middleware and integrations push into.
    def push(event)
      ensure_forked!
      @queue.push(event)
    end
    alias << push

    def flush
      @flusher&.flush_now
    end

    def shutdown
      @flusher&.stop
      @flusher = nil
    end

    # Re-spawn flusher in a forked child. Hosted servers (Puma clustered,
    # Unicorn, Passenger) MUST call this in their on_worker_boot hook.
    # Beacon detects forks lazily on the next push too — but explicit is
    # cheaper than waiting for the first event.
    def after_fork
      @mutex.synchronize do
        @pid     = Process.pid
        @queue   = Beacon::Queue.new(max: @config.queue_size)
        @flusher = nil
        start_flusher if @config.async
      end
    end

    private

    def ensure_forked!
      return if @pid == Process.pid
      after_fork
    end

    def start_flusher
      @flusher = Flusher.new(self, transport: @transport)
      @flusher.start
    end

    def extract_actor(props)
      user = props.delete(:user)
      return [nil, nil] unless user
      type = user.class.name
      id   = user.respond_to?(:id) ? user.id : nil
      [type, id]
    end

    def base_context
      @base_context ||= {
        environment: @config.environment,
        deploy_sha:  @config.deploy_sha,
        language:    LANGUAGE,
      }.compact.freeze
    end

    def realtime_ns
      Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond)
    end
  end
end
