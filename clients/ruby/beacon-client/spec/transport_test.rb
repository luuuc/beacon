require "test_helper"
require "socket"
require "beacon/transport"

# Tiny HTTP/1.1 test server speaking just enough to exercise
# Beacon::Transport::Http's persistent-connection and reconnect paths.
#
# It listens on 127.0.0.1 on an OS-assigned port. Each accepted connection
# increments a counter so tests can assert "exactly one connection for N
# sequential POSTs." A script of responses controls per-request behavior:
# :ok returns 202, :close_mid_request closes the socket before sending a
# response (the client sees EOFError/EPIPE).
class FakeHttpServer
  attr_reader :port, :accept_count, :request_count, :last_headers

  def initialize
    @server        = TCPServer.new("127.0.0.1", 0)
    @port          = @server.addr[1]
    @accept_count  = 0
    @request_count = 0
    @last_headers  = {}
    @script        = []
    @mutex         = Mutex.new
    @thread        = Thread.new { accept_loop }
    @thread.name   = "fake-http-server" if @thread.respond_to?(:name=)
  end

  def script(*behaviors)
    @mutex.synchronize { @script.concat(behaviors) }
  end

  def stop
    @server.close rescue nil
    @thread&.join(1)
  end

  private

  def accept_loop
    loop do
      socket = @server.accept
      @mutex.synchronize { @accept_count += 1 }
      # Each accepted connection is handled in its own thread so one
      # hung/keep-alive connection doesn't block subsequent accepts.
      # Matters for the reconnect tests: while connection #1 is
      # hanging on :hang_forever, the client must be able to open
      # connection #2.
      Thread.new(socket) { |s| handle(s) }
    end
  rescue IOError, Errno::EBADF
    # server closed
  end

  def handle(socket)
    loop do
      request_line = socket.gets
      break if request_line.nil?  # peer closed the connection
      headers = {}
      content_length = 0
      while (line = socket.gets) && line != "\r\n"
        if line =~ /\A([^:]+):\s*(.+?)\r\n\z/
          headers[$1.downcase] = $2
          content_length = $2.to_i if $1.downcase == "content-length"
        end
      end
      socket.read(content_length) if content_length > 0

      @mutex.synchronize do
        @request_count += 1
        @last_headers = headers
      end
      behavior = @mutex.synchronize { @script.shift } || :ok

      case behavior
      when :close_mid_request
        # Close without writing a response — simulates server restart /
        # dead keep-alive socket. The client sees EOFError/EPIPE.
        socket.close
        return
      when :hang_forever
        # Read the request, then never write a response. The client
        # will hit its read_timeout and raise Net::ReadTimeout.
        sleep
      when :ok
        socket.write("HTTP/1.1 202 Accepted\r\n")
        socket.write("Content-Length: 0\r\n")
        socket.write("Connection: keep-alive\r\n")
        socket.write("\r\n")
      end
    end
  rescue IOError, Errno::EPIPE, Errno::ECONNRESET
    # client gave up
  ensure
    socket.close rescue nil
  end
end

class TransportTest < Minitest::Test
  def setup
    Beacon.reset_config!
    @server = FakeHttpServer.new
    Beacon.configure do |c|
      c.endpoint        = "http://127.0.0.1:#{@server.port}"
      c.connect_timeout = 0.2
      c.read_timeout    = 0.2
      c.async           = false
    end
    @transport = Beacon::Transport::Http.new(Beacon.config)
  end

  def teardown
    # Drop the client's end of the keep-alive first so the server's
    # handle loop wakes from `socket.gets` on EOF and the accept thread
    # can exit cleanly. Otherwise @thread.join spins for a full second.
    @transport.after_fork rescue nil
    @server.stop
    Beacon.reset_config!
  end

  def test_sets_user_agent_header
    @transport.post("{}", idempotency_key: "k")
    assert_match(%r{\Abeacon-client/\d+\.\d+\.\d+ \(ruby \d},
      @server.last_headers["user-agent"].to_s)
  end

  def test_sets_idempotency_key_header
    @transport.post("{}", idempotency_key: "abc-123")
    assert_equal "abc-123", @server.last_headers["idempotency-key"]
  end

  def test_reuses_one_connection_across_many_sequential_posts
    100.times do |i|
      result = @transport.post("{}", idempotency_key: "k#{i}")
      assert_equal 202, result.status, "POST #{i} should succeed"
    end
    assert_equal 1, @server.accept_count,
      "expected one accepted connection across 100 sequential POSTs, got #{@server.accept_count}"
    assert_equal 100, @server.request_count
  end

  def test_reconnects_once_on_broken_pipe_and_retries
    @server.script(:close_mid_request)
    # First POST: server closes mid-request → client reconnects → retry
    # succeeds.
    result = @transport.post("{}", idempotency_key: "k1")
    assert_equal 202, result.status
    assert_equal 2, @server.accept_count,
      "expected a second connection after the reconnect"
  end

  def test_post_surfaces_transport_error_when_both_attempts_fail
    # Script: the first request gets closed mid-stream → client retries
    # once → the second attempt also gets closed mid-stream → no more
    # retries → the error propagates into post's rescue and becomes a
    # Result transport error.
    @server.script(:close_mid_request, :close_mid_request)
    result = @transport.post("{}", idempotency_key: "k")
    assert result.transport_error?,
      "expected transport error after both attempts failed, got status #{result.status}"
    assert_equal 0, result.status
  end

  def test_reconnects_once_on_read_timeout_and_retries
    # First request hangs past the read_timeout → Net::ReadTimeout →
    # client closes the dead socket → reconnects → retry succeeds.
    @server.script(:hang_forever)
    result = @transport.post("{}", idempotency_key: "k")
    assert_equal 202, result.status
    assert_equal 2, @server.accept_count,
      "expected a second connection after the timeout-driven reconnect"
  end

  def test_after_fork_drops_held_connection
    @transport.post("{}", idempotency_key: "k")
    refute_nil @transport.instance_variable_get(:@http),
      "warmup should have established a connection"
    @transport.after_fork
    assert_nil @transport.instance_variable_get(:@http),
      "after_fork must drop the held connection"
    # Next post reopens cleanly.
    result = @transport.post("{}", idempotency_key: "k2")
    assert_equal 202, result.status
  end
end
