require "test_helper"
require "socket"
require "beacon"
require "beacon/transport"

# This test simulates the Puma-clustered case: the parent boots a Beacon
# client (with a real flusher thread), then forks a worker, then the worker
# pushes events. After Client#after_fork the child must:
#   - have its own queue (not see the parent's events)
#   - have a live flusher thread
#   - not double-flush the parent's events
class ForkSafetyTest < Minitest::Test
  def setup
    skip "Process.fork not available" unless Process.respond_to?(:fork)
    Beacon::Testing.reset_config!
    Beacon.configure do |c|
      c.async          = true
      c.flush_interval = 0.05
    end
    @transport = Beacon::Testing::FakeTransport.new
    @client    = Beacon::Client.new(config: Beacon.config, transport: @transport)
  end

  def teardown
    @client&.shutdown
    Beacon::Testing.reset_config!
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

  def test_persistent_transport_opens_a_fresh_socket_in_fork_child
    # Real fork-safety test for Card 4's persistent connection. The
    # parent warms up a POST (opens a socket), then forks; the child
    # calls after_fork, posts, and reports the accept_count seen by
    # the test server. If Transport::Http#after_fork is missing or
    # misbehaves, parent and child will share the same FD and the
    # server will see exactly one accept across both; we assert two.
    server = nil
    accept_count = nil
    begin
      server = TCPServer.new("127.0.0.1", 0)
      port   = server.addr[1]
      accept_count_ivar = [0]
      accept_mutex      = Mutex.new
      server_thread = Thread.new do
        loop do
          socket = server.accept rescue break
          accept_mutex.synchronize { accept_count_ivar[0] += 1 }
          # Handle each connection in its own thread so the parent's
          # keep-alive sitting in gets can't block us from accepting
          # the child's post-fork connection.
          Thread.new(socket) do |s|
            begin
              loop do
                line = s.gets
                break if line.nil?
                clen = 0
                while (h = s.gets) && h != "\r\n"
                  clen = $1.to_i if h =~ /\AContent-Length:\s*(\d+)\r\n\z/i
                end
                s.read(clen) if clen > 0
                s.write("HTTP/1.1 202 Accepted\r\nContent-Length: 0\r\nConnection: keep-alive\r\n\r\n")
              end
            rescue IOError, Errno::EPIPE, Errno::ECONNRESET
              # client gave up
            ensure
              s.close rescue nil
            end
          end
        end
      end

      Beacon.config.endpoint        = "http://127.0.0.1:#{port}"
      Beacon.config.connect_timeout = 0.5
      Beacon.config.read_timeout    = 0.5
      parent_transport = Beacon::Transport::Http.new(Beacon.config)
      parent_client    = Beacon::Client.new(
        config: Beacon.config, transport: parent_transport, autostart: false,
      )
      # Warm up the parent: first POST opens socket #1.
      parent_transport.post("{}", idempotency_key: "parent")

      read, write = IO.pipe
      pid = Process.fork do
        read.close
        begin
          parent_client.after_fork
          # The Client#after_fork call should have dropped the held
          # socket. The child's first POST must open a fresh one —
          # socket #2 from the server's perspective.
          parent_transport.post("{}", idempotency_key: "child")
        ensure
          write.write("done")
          write.close
          exit!(0)
        end
      end
      write.close
      Process.wait(pid)
      read.read; read.close

      # Drop the parent's keep-alive so the handler thread wakes from
      # gets and exits, then close the listener so the accept thread
      # bails out of its loop.
      parent_transport.after_fork
      server.close
      server_thread.join(1)
      accept_count = accept_mutex.synchronize { accept_count_ivar[0] }
    ensure
      server&.close rescue nil
    end

    assert_operator accept_count, :>=, 2,
      "expected at least 2 accepts (parent warmup + child post-fork); " \
      "got #{accept_count}. A value of 1 means the child inherited the " \
      "parent's socket FD — Transport::Http#after_fork is broken."
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
