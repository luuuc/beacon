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
      def self.install(client: Beacon.client)
        unless defined?(::ActiveSupport::Notifications)
          warn "[beacon] ActionMailer integration requires ActiveSupport — skipping"
          return
        end

        ::ActiveSupport::Notifications.subscribe("deliver.action_mailer") do |_name, _start, _finish, _id, payload|
          exception = payload[:exception_object]
          next unless exception
          begin
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
