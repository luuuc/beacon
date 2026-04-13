require "beacon"

# ActiveJob integration. Opt-in:
#
#   # config/initializers/beacon.rb
#   require "beacon/integrations/active_job"
#   Beacon::Integrations::ActiveJob.install
#
# Subscribes to "perform.active_job" notifications and emits a perf event
# per job. Requires ActiveSupport::Notifications — host apps that already
# pull in Rails get this for free; the gem itself does not depend on it.
module Beacon
  module Integrations
    module ActiveJob
      def self.install(client: Beacon.client, config: Beacon.config)
        unless defined?(::ActiveSupport::Notifications)
          warn "[beacon] ActiveJob integration requires ActiveSupport — skipping"
          return
        end

        ambient = config.ambient

        ::ActiveSupport::Notifications.subscribe("perform.active_job") do |_name, started, finished, _id, payload|
          begin
            duration_ms = ((finished - started) * 1000).to_i
            job = payload[:job]
            client.push({
              kind:          :perf,
              name:          job.class.name,
              created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
              properties: {
                duration_ms: duration_ms,
                queue:       job.queue_name,
                success:     payload[:exception_object].nil?,
              },
            })

            if ambient
              client.push({
                kind:          :ambient,
                name:          "job_lifecycle",
                created_at_ns: Process.clock_gettime(Process::CLOCK_REALTIME, :nanosecond),
                properties: {
                  class:       job.class.name,
                  queue:       job.queue_name,
                  duration_ms: duration_ms,
                  status:      payload[:exception_object].nil? ? "success" : "failure",
                },
              })
            end
          rescue => e
            warn "[beacon] active_job subscriber rescued #{e.class}: #{e.message}"
          end
        end
      end
    end
  end
end
