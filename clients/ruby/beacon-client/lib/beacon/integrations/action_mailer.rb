require "beacon"

# ActionMailer integration. Captures delivery errors as Beacon error events
# via the same fingerprint algorithm the Rack middleware uses.
#
#   # config/initializers/beacon.rb
#   require "beacon/integrations/action_mailer"
#   Beacon::Integrations::ActionMailer.install
module Beacon
  module Integrations
    module ActionMailer
      def self.install(client: Beacon.client, config: Beacon.config)
        unless defined?(::ActiveSupport::Notifications)
          warn "[beacon] ActionMailer integration requires ActiveSupport — skipping"
          return
        end

        ambient = config.ambient

        ::ActiveSupport::Notifications.subscribe("deliver.action_mailer") do |_name, _start, _finish, _id, payload|
          begin
            # Ambient: emit a mailer_delivery event for every delivery attempt.
            if ambient
              mailer = payload[:mailer] || "UnknownMailer"
              action = payload[:action] || "unknown"
              status = payload[:exception_object].nil? ? "success" : "failure"
              client.push({
                kind:          :ambient,
                name:          "mailer_delivery",
                created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
                properties: {
                  mailer: mailer,
                  action: action,
                  status: status,
                },
              })
            end

            # Error capture: only on failure.
            exception = payload[:exception_object]
            next unless exception

            frame       = (exception.backtrace || []).first.to_s
            fingerprint = Beacon::Fingerprint.compute(exception.class.name, frame)
            client.push({
              kind:          :error,
              name:          exception.class.name,
              created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
              properties: {
                fingerprint:     fingerprint,
                message:         exception.message.to_s[0, 500],
                first_app_frame: frame,
                stack_trace:     exception.backtrace&.join("\n"),
              },
            })
          rescue => e
            warn "[beacon] action_mailer subscriber rescued #{e.class}: #{e.message}"
          end
        end
      end
    end
  end
end
