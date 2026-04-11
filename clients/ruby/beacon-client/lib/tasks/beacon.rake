# Rake tasks shipped with the beacon-client gem.
#
# Auto-loaded into the host Rails app by Beacon::Railtie's rake_tasks
# block (see lib/beacon/rails.rb) so `rails -T beacon` lists them
# without any setup step in the host app.

namespace :beacon do
  desc "Fire a deploy.shipped outcome event tagged with the current " \
       "deployment version (reads KAMAL_VERSION, falling back to " \
       "GIT_SHA). Intended for .kamal/hooks/post-deploy or an " \
       "equivalent post-deploy step in your pipeline — not for " \
       "per-process boot hooks. A single rake task invocation fires " \
       "exactly one deploy.shipped event and synchronously flushes " \
       "the queue before exiting so Beacon's rollup worker always " \
       "sees the event."
  task deploy_shipped: :environment do
    version = ENV["KAMAL_VERSION"]
    version = ENV["GIT_SHA"] if version.nil? || version.empty?

    if version.nil? || version.empty?
      warn "[beacon] beacon:deploy_shipped requires KAMAL_VERSION or " \
           "GIT_SHA in the environment. Kamal sets KAMAL_VERSION " \
           "automatically in post-deploy hooks; if you're running " \
           "this task by hand, set GIT_SHA=$(git rev-parse --short HEAD) " \
           "first. Aborting without firing the event."
      exit 1
    end

    environment = ENV["BEACON_ENVIRONMENT"] ||
                  ENV["RAILS_ENV"] ||
                  (defined?(::Rails) && ::Rails.respond_to?(:env) ? ::Rails.env.to_s : nil) ||
                  "production"

    Beacon.track("deploy.shipped",
      version:     version,
      environment: environment,
    )

    # Synchronous drain. The rake task exits as soon as the block
    # returns, which is BEFORE the background flusher's next
    # time-triggered tick — without this, the event sits in the
    # bounded queue for 1s and then is lost when the process dies.
    Beacon.flush

    puts "[beacon] fired deploy.shipped version=#{version} environment=#{environment}"
  end
end
