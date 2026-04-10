require "test_helper"
require "beacon"
require "beacon/test/fake_transport"

# This test simulates the Puma-clustered case: the parent boots a Beacon
# client (with a real flusher thread), then forks a worker, then the worker
# pushes events. After Client#after_fork the child must:
#   - have its own queue (not see the parent's events)
#   - have a live flusher thread
#   - not double-flush the parent's events
class ForkSafetyTest < Minitest::Test
  def setup
    skip "Process.fork not available" unless Process.respond_to?(:fork)
    Beacon.reset_config!
    Beacon.configure do |c|
      c.async          = true
      c.flush_interval = 0.05
    end
    @transport = Beacon::Test::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport)
  end

  def teardown
    @client&.shutdown
    Beacon.reset_config!
  end

  def test_child_has_isolated_queue_and_live_flusher
    # Push two events in the parent — the parent flusher will eat these.
    @client.track("parent.event1")
    @client.track("parent.event2")
    parent_thread_id = @client.flusher.instance_variable_get(:@thread).object_id

    read, write = IO.pipe
    pid = Process.fork do
      read.close
      begin
        @client.after_fork

        child_queue_size_after_fork = @client.queue.length
        flusher_alive               = @client.flusher.alive?
        child_thread_id             = @client.flusher.instance_variable_get(:@thread).object_id
        same_thread                 = (child_thread_id == parent_thread_id)

        @client.track("child.event1")
        @client.track("child.event2")
        @client.track("child.event3")
        child_queue_size_after_pushes = @client.queue.length

        write.write([
          child_queue_size_after_fork,
          flusher_alive ? 1 : 0,
          same_thread ? 1 : 0,
          child_queue_size_after_pushes,
        ].join(","))
      ensure
        write.close
        exit!(0)
      end
    end
    write.close
    Process.wait(pid)
    result = read.read
    read.close

    fork_size, alive, same_thread, after_pushes = result.split(",").map(&:to_i)
    assert_equal 0, fork_size,         "child queue should be empty after fork"
    assert_equal 1, alive,             "child should have a live flusher thread"
    assert_equal 0, same_thread,       "child flusher must be a NEW thread, not the parent's"
    assert_equal 3, after_pushes,      "child should accept its own pushes"
  end

  def test_lazy_fork_detection_on_push_without_after_fork
    @client.track("parent.event")

    read, write = IO.pipe
    pid = Process.fork do
      read.close
      begin
        # Skip after_fork — push should still detect the fork lazily.
        @client.track("child.event")
        write.write(@client.queue.length.to_s)
      ensure
        write.close
        exit!(0)
      end
    end
    write.close
    Process.wait(pid)
    size = read.read.to_i
    read.close
    assert_equal 1, size, "lazy fork detection should reset queue on first push"
  end
end
