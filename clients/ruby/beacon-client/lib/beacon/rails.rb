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
# Route-template integration (writing env["beacon.route_template"] from
# AS::Notifications) lives separately and is installed by Card 2.
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
