package main

import (
	"bufio"
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultEndpoint    = "http://127.0.0.1:4681/rpc"
	proxyScannerBufMax = 1 << 20 // 1 MB
	proxyHTTPTimeout   = 30 * time.Second
)

// cmdMCP dispatches `beacon mcp <subcommand>`.
func cmdMCP(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "beacon mcp: subcommand required (proxy)")
		return 2
	}
	switch args[0] {
	case "proxy":
		return cmdMCPProxy(args[1:], os.Stdin, stdout, stderr, os.Getenv)
	default:
		fmt.Fprintf(stderr, "beacon mcp: unknown subcommand %q\n", args[0])
		return 2
	}
}

// cmdMCPProxy reads newline-delimited JSON-RPC from stdin, POSTs each
// message to a Beacon MCP endpoint with bearer auth, and writes the
// response to stdout. Logging goes to stderr.
//
// The proxy is a pipe, not a service — it uses log.New (not slog) because
// structured logging to stderr would be noise for a stdio transport.
func cmdMCPProxy(args []string, stdin io.Reader, stdout, stderr io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("mcp proxy", flag.ContinueOnError)
	fs.SetOutput(stderr)
	token := fs.String("token", "", "bearer token (overrides BEACON_AUTH_TOKEN)")
	fs.Usage = func() {
		fmt.Fprintf(stderr, "Usage: beacon mcp proxy [flags] [endpoint]\n\n")
		fmt.Fprintf(stderr, "Stdio-to-HTTP proxy for MCP clients (Claude Code, Claude Desktop, Cursor).\n")
		fmt.Fprintf(stderr, "Reads JSON-RPC from stdin, POSTs to the endpoint, writes responses to stdout.\n\n")
		fmt.Fprintf(stderr, "Endpoint defaults to %s if omitted.\n\n", defaultEndpoint)
		fmt.Fprintf(stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}

	endpoint := defaultEndpoint
	if fs.NArg() > 0 {
		endpoint = fs.Arg(0)
	}

	authToken := *token
	if authToken == "" {
		authToken = getenv("BEACON_AUTH_TOKEN")
	}

	logger := log.New(stderr, "beacon mcp proxy: ", 0)

	client := &http.Client{Timeout: proxyHTTPTimeout}

	scanner := bufio.NewScanner(stdin)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, proxyScannerBufMax)

	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}

		req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader([]byte(line)))
		if err != nil {
			logger.Printf("build request: %v", err)
			return 1
		}
		req.Header.Set("Content-Type", "application/json")
		if authToken != "" {
			req.Header.Set("Authorization", "Bearer "+authToken)
		}

		resp, err := client.Do(req)
		if err != nil {
			logger.Printf("POST %s: %v", endpoint, err)
			return 1
		}

		if resp.StatusCode == http.StatusUnauthorized {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			logger.Printf("POST %s: 401 unauthorized", endpoint)
			return 1
		}

		// Drain response body to stdout. The upstream response from
		// json.NewEncoder already includes a trailing newline, so we
		// don't add another — one JSON object per line, no blanks.
		_, copyErr := io.Copy(stdout, resp.Body)
		resp.Body.Close()
		if copyErr != nil {
			logger.Printf("read response: %v", copyErr)
			return 1
		}
	}

	if err := scanner.Err(); err != nil {
		logger.Printf("stdin: %v", err)
		return 1
	}

	// Clean EOF.
	return 0
}
