require "test_helper"
require "rake"
require "stringio"

# Tests for the shipped rake tasks under lib/tasks/beacon.rake. These
# are the tasks a host Rails app runs from a Kamal post-deploy hook
# (or equivalent) to fire the deploy.shipped outcome event.
#
# Each test gets its own Rake::Application instance so task
# registration doesn't leak between tests and doesn't fight with
# whatever the host project's Rakefile loaded before the suite ran.
class BeaconRakeTasksTest < Minitest::Test
  RAKE_PATH = File.expand_path("../lib/tasks/beacon.rake", __dir__).freeze

  def setup
    # Save env we intend to mutate so teardown can restore.
    @orig_env = {
      "KAMAL_VERSION"     => ENV["KAMAL_VERSION"],
      "GIT_SHA"           => ENV["GIT_SHA"],
      "BEACON_DISABLED"   => ENV["BEACON_DISABLED"],
      "BEACON_ENVIRONMENT"=> ENV["BEACON_ENVIRONMENT"],
      "RAILS_ENV"         => ENV["RAILS_ENV"],
      "RACK_ENV"          => ENV["RACK_ENV"],
    }
    # Force Beacon into its kill-switched no-op state so the task's
    # Beacon.track + Beacon.flush are safe no-ops. Otherwise the task
    # would try to push an event through a real transport at
    # http://127.0.0.1:4680 which isn't running.
    ENV["BEACON_DISABLED"] = "1"
    ENV.delete("KAMAL_VERSION")
    ENV.delete("GIT_SHA")
    ENV.delete("BEACON_ENVIRONMENT")
    ENV.delete("RAILS_ENV")
    ENV.delete("RACK_ENV")
    Beacon::Testing.reset_config!

    # Fresh Rake application — isolates task registration so
    # re-loading the rake file in each test doesn't duplicate-define.
    @prev_rake = Rake.application
    Rake.application = Rake::Application.new
    load RAKE_PATH
    # The task depends on :environment (Rails's rake task that boots
    # the app). We don't have Rails here, so drop the prerequisite.
    Rake::Task["beacon:deploy_shipped"].clear_prerequisites
  end

  def teardown
    Rake.application = @prev_rake
    @orig_env.each { |k, v| v.nil? ? ENV.delete(k) : ENV[k] = v }
    Beacon::Testing.reset_config!
  end

  def test_kamal_version_is_picked_up_and_printed
    ENV["KAMAL_VERSION"] = "abc1234"
    out = capture_stdout { Rake::Task["beacon:deploy_shipped"].invoke }
    assert_match(/version=abc1234/, out)
    assert_match(/deploy\.shipped/, out)
  end

  def test_falls_back_to_git_sha_when_kamal_version_missing
    ENV["GIT_SHA"] = "def5678"
    out = capture_stdout { Rake::Task["beacon:deploy_shipped"].invoke }
    assert_match(/version=def5678/, out)
  end

  def test_kamal_version_wins_over_git_sha_when_both_present
    ENV["KAMAL_VERSION"] = "kamal_value"
    ENV["GIT_SHA"]       = "git_value"
    out = capture_stdout { Rake::Task["beacon:deploy_shipped"].invoke }
    assert_match(/version=kamal_value/, out)
    refute_match(/git_value/, out)
  end

  def test_aborts_with_nonzero_exit_when_no_version_available
    assert_raises(SystemExit) do
      capture_stdout(stderr: true) { Rake::Task["beacon:deploy_shipped"].invoke }
    end
  end

  def test_aborts_with_helpful_message_pointing_at_kamal_version
    ENV.delete("KAMAL_VERSION")
    ENV.delete("GIT_SHA")
    err = capture_stderr do
      begin
        Rake::Task["beacon:deploy_shipped"].invoke
      rescue SystemExit
        # expected
      end
    end
    assert_match(/KAMAL_VERSION/, err)
    assert_match(/GIT_SHA/, err)
  end

  def test_environment_defaults_to_beacon_environment_env_var_if_set
    ENV["KAMAL_VERSION"]      = "v1"
    ENV["BEACON_ENVIRONMENT"] = "staging"
    out = capture_stdout { Rake::Task["beacon:deploy_shipped"].invoke }
    assert_match(/environment=staging/, out)
  end

  def test_environment_falls_back_to_rails_env_if_beacon_env_unset
    ENV["KAMAL_VERSION"] = "v1"
    ENV["RAILS_ENV"]     = "production"
    out = capture_stdout { Rake::Task["beacon:deploy_shipped"].invoke }
    assert_match(/environment=production/, out)
  end

  private

  def capture_stdout(stderr: false)
    orig_stdout, orig_stderr = $stdout, $stderr
    $stdout = StringIO.new
    $stderr = StringIO.new if stderr
    yield
    $stdout.string
  ensure
    $stdout = orig_stdout
    $stderr = orig_stderr if stderr
  end

  def capture_stderr
    orig_stderr = $stderr
    $stderr = StringIO.new
    yield
    $stderr.string
  ensure
    $stderr = orig_stderr
  end
end
