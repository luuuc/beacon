require "beacon"
require "beacon/middleware"

# Rails integration for Beacon. Loaded automatically by lib/beacon.rb when
# Rails::Railtie is defined — host apps do not `require` this file directly.
#
# Responsibilities:
#
#   - Set config.app_root = Rails.root before user config runs, so the
#     middleware's first_app_frame filter resolves user code correctly.
#   - Insert Beacon::Middleware into the Rails middleware stack.
#   - Auto-install the ActiveJob and ActionMailer integrations so host apps
#     don't have to `require "beacon/integrations/..."`.
#   - Install a Process._fork hook that calls Beacon.client.after_fork in
#     the child, covering clustered Puma, Unicorn, and Passenger without
#     touching their internals.
#
# Route templates are written into env["beacon.route_template"] from a
# start_processing.action_controller subscriber so the middleware's name
# lookup short-circuits the regex path normalizer for Rails requests.
# The non-Rails regex fallback (PathNormalizer) stays in place as the
# documented fallback for plain Rack apps.
module Beacon
  class Railtie < ::Rails::Railtie
    config.before_configuration do
      Beacon.config.app_root = ::Rails.root.to_s if defined?(::Rails) && ::Rails.root
    end

    # Insertion point: immediately after ActionDispatch::DebugExceptions.
    #
    # Rails' middleware stack (outermost to app) looks roughly like:
    #
    #   ActionDispatch::DebugExceptions  ← renders error pages
    #   ActionDispatch::ShowExceptions   ← catches + renders public error pages
    #   ...
    #   Beacon::Middleware               ← must sit INSIDE the exception wall
    #   ...
    #   Rails router
    #
    # With Beacon after DebugExceptions, a controller exception bubbles up
    # through Beacon first (which records it and re-raises) and then through
    # DebugExceptions (which renders). If Beacon sat *outside* DebugExceptions,
    # the exception would already be swallowed into a rendered response by
    # the time Beacon saw it, and `capture_error` would never fire.
    initializer "beacon.insert_middleware", before: :build_middleware_stack do |app|
      next unless Beacon.config.pillar?(:perf) || Beacon.config.pillar?(:errors)
      if defined?(::ActionDispatch::DebugExceptions)
        app.middleware.insert_after ::ActionDispatch::DebugExceptions, Beacon::Middleware
      else
        app.middleware.use Beacon::Middleware
      end
    end

    # Route template subscriber. Fires at the start of every controller
    # action (after routing, before the action runs) and writes the
    # matched route template into the request env so Beacon::Middleware
    # — which sits outside the router — can read it when the action
    # returns.
    #
    # Shape: "<METHOD> <path-template>", e.g. "GET /users/:id" — same
    # format as the PathNormalizer fallback and the cross-client
    # contract pinned in spec/fixtures.json's path_normalization
    # section. The middleware trusts this string verbatim.
    #
    # Source of truth: Rails 7.1+ stashes the matched route's URI
    # pattern on request.route_uri_pattern (a method on the request,
    # not an env key). The matched pattern is set on the request
    # instance by the router.
    initializer "beacon.subscribe_action_controller", after: :load_config_initializers do |_app|
      next unless defined?(::ActiveSupport::Notifications)
      Beacon::Railtie.instance_variable_set(:@subscriber_log_throttle, Beacon::LogThrottle.new)
      ::ActiveSupport::Notifications.subscribe("start_processing.action_controller") do |_name, _start, _finish, _id, payload|
        begin
          request = payload[:request]
          env     = request&.env || payload[:headers]&.env
          next unless env
          template = Beacon::Railtie.route_template_for(env, payload)
          env["beacon.route_template"] = template if template
        rescue => e
          Beacon::Railtie.instance_variable_get(:@subscriber_log_throttle).warn(:"subscriber_#{e.class.name}") do |count|
            suffix = count > 1 ? " (#{count} in the last minute)" : ""
            "action_controller subscriber rescued #{e.class}: #{e.message}#{suffix}"
          end
        end
      end
    end

    initializer "beacon.install_integrations", after: :load_config_initializers do |_app|
      if defined?(::ActiveJob)
        require "beacon/integrations/active_job"
        Beacon::Integrations::ActiveJob.install
      end
      if defined?(::ActionMailer)
        require "beacon/integrations/action_mailer"
        Beacon::Integrations::ActionMailer.install
      end
    end

    initializer "beacon.install_fork_hook", after: :load_config_initializers do |_app|
      next unless Process.respond_to?(:_fork)
      next if Beacon::Railtie.instance_variable_get(:@fork_hook_installed)
      Beacon::Railtie.instance_variable_set(:@fork_hook_installed, true)
      Process.singleton_class.prepend(ForkHook)
    end

    ROUTE_FORMAT_SUFFIX = "(.:format)".freeze

    # Extract "<METHOD> <path-template>" from a start_processing payload.
    # Returns nil when no template is available — the middleware then
    # falls through to Beacon::PathNormalizer, which is the documented
    # cross-client fallback and emits the same shape. We deliberately
    # do not emit a controller#action string here: mixing two label
    # shapes on one dashboard produces confusing groupings.
    #
    # Source of truth: ActionDispatch::Request#route_uri_pattern
    # (Rails 7.1+). This is a *method* on the request object, not an
    # env key — the matched pattern is stored on the request instance
    # by the router and read via the public accessor.
    def self.route_template_for(env, payload)
      method  = env["REQUEST_METHOD"]
      request = payload[:request]
      return nil unless method && request && request.respond_to?(:route_uri_pattern)

      pattern = request.route_uri_pattern
      return nil unless pattern && !pattern.empty?

      # Strip the "(.:format)" tail without allocating on the common path.
      if pattern.end_with?(ROUTE_FORMAT_SUFFIX)
        pattern = pattern[0, pattern.length - ROUTE_FORMAT_SUFFIX.length]
      end
      "#{method} #{pattern}"
    end

    # Ruby 3.1+ Process._fork hook. Runs in every fork child, which is
    # exactly what we want — the first push in a new worker would re-init
    # anyway via Client#ensure_forked!, but this makes it eager so the
    # flusher is already running when the first request lands.
    module ForkHook
      def _fork
        pid = super
        if pid == 0
          begin
            Beacon.client.after_fork
          rescue => e
            warn "[beacon] after_fork hook rescued #{e.class}: #{e.message}"
          end
        end
        pid
      end
    end
  end
end
