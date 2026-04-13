package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/luuuc/beacon/internal/beacondb/memfake"
	"github.com/luuuc/beacon/internal/mcpserver"
	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/rollup"
)

// noEnv is a getenv stub that returns "" for every key.
func noEnv(string) string { return "" }

// envMap returns a getenv stub that looks up keys in the given map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeRPC is a minimal JSON-RPC responder for proxy unit tests.
func fakeRPC(wantToken string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if wantToken != "" {
			if r.Header.Get("Authorization") != "Bearer "+wantToken {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "beacon", "version": "test"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		default:
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"error":   map[string]any{"code": -32601, "message": "method not found"},
			})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}
}

func rpcLine(method string) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	})
	return string(b) + "\n"
}

func rpcLineWithArgs(method string, params map[string]any) string {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	})
	return string(b) + "\n"
}

// ---------------------------------------------------------------------------
// Unit tests (fake RPC server)
// ---------------------------------------------------------------------------

func TestProxy_HappyPath(t *testing.T) {
	srv := httptest.NewServer(fakeRPC(""))
	defer srv.Close()

	stdin := strings.NewReader(
		rpcLine("initialize") +
			rpcLine("tools/list"),
	)
	var stdout, stderr bytes.Buffer

	code := cmdMCPProxy([]string{srv.URL}, stdin, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}

	// Two newline-delimited JSON responses, no blank lines between them.
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d response lines, want 2:\n%q", len(lines), stdout.String())
	}

	// Verify first response is a valid initialize result.
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("decode line 0: %v", err)
	}
	if resp.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q", resp.Result.ProtocolVersion)
	}
}

func TestProxy_AuthFailure(t *testing.T) {
	srv := httptest.NewServer(fakeRPC("secret"))
	defer srv.Close()

	stdin := strings.NewReader(rpcLine("initialize"))
	var stdout, stderr bytes.Buffer

	code := cmdMCPProxy([]string{srv.URL}, stdin, &stdout, &stderr, noEnv)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "401") {
		t.Errorf("stderr should mention 401: %s", stderr.String())
	}
}

func TestProxy_AuthSuccess_Flag(t *testing.T) {
	srv := httptest.NewServer(fakeRPC("mytoken"))
	defer srv.Close()

	stdin := strings.NewReader(rpcLine("initialize"))
	var stdout, stderr bytes.Buffer

	code := cmdMCPProxy([]string{"--token", "mytoken", srv.URL}, stdin, &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}
}

func TestProxy_AuthSuccess_EnvVar(t *testing.T) {
	srv := httptest.NewServer(fakeRPC("envtoken"))
	defer srv.Close()

	stdin := strings.NewReader(rpcLine("initialize"))
	var stdout, stderr bytes.Buffer

	env := envMap(map[string]string{"BEACON_AUTH_TOKEN": "envtoken"})
	code := cmdMCPProxy([]string{srv.URL}, stdin, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}
}

func TestProxy_FlagOverridesEnvVar(t *testing.T) {
	srv := httptest.NewServer(fakeRPC("flagtoken"))
	defer srv.Close()

	stdin := strings.NewReader(rpcLine("initialize"))
	var stdout, stderr bytes.Buffer

	env := envMap(map[string]string{"BEACON_AUTH_TOKEN": "wrongtoken"})
	code := cmdMCPProxy([]string{"--token", "flagtoken", srv.URL}, stdin, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}
}

func TestProxy_ServerUnreachable(t *testing.T) {
	stdin := strings.NewReader(rpcLine("initialize"))
	var stdout, stderr bytes.Buffer

	// Use a port that nothing listens on.
	code := cmdMCPProxy([]string{"http://127.0.0.1:1"}, stdin, &stdout, &stderr, noEnv)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if stderr.Len() == 0 {
		t.Error("expected error on stderr")
	}
}

func TestProxy_EOFOnStdin(t *testing.T) {
	srv := httptest.NewServer(fakeRPC(""))
	defer srv.Close()

	// Empty stdin → immediate EOF.
	var stdout, stderr bytes.Buffer
	code := cmdMCPProxy([]string{srv.URL}, io.LimitReader(strings.NewReader(""), 0), &stdout, &stderr, noEnv)
	if code != 0 {
		t.Fatalf("exit %d, want 0; stderr: %s", code, stderr.String())
	}
}

func TestProxy_ScannerOverflow(t *testing.T) {
	srv := httptest.NewServer(fakeRPC(""))
	defer srv.Close()

	// Send a single line that exceeds the 1 MB scanner buffer.
	huge := `{"jsonrpc":"2.0","id":1,"method":"initialize","padding":"` +
		strings.Repeat("x", proxyScannerBufMax+100) +
		`"}` + "\n"

	var stdout, stderr bytes.Buffer
	code := cmdMCPProxy([]string{srv.URL}, strings.NewReader(huge), &stdout, &stderr, noEnv)
	if code != 1 {
		t.Fatalf("exit %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "stdin:") {
		t.Errorf("stderr should log scanner error: %s", stderr.String())
	}
}

func TestProxy_UsageText(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := cmdMCPProxy([]string{"-h"}, strings.NewReader(""), &stdout, &stderr, noEnv)
	if code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
	out := stderr.String()
	if !strings.Contains(out, "endpoint") {
		t.Errorf("usage should mention endpoint: %s", out)
	}
	if !strings.Contains(out, "Stdio-to-HTTP") {
		t.Errorf("usage should describe the proxy: %s", out)
	}
}

// ---------------------------------------------------------------------------
// Integration test (real MCP server, in-process)
// ---------------------------------------------------------------------------

func TestProxy_Integration_RealMCPServer(t *testing.T) {
	// Stand up a real MCP server backed by memfake.
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	readsH := reads.NewHandler(reads.Config{}, fake, nil)
	worker := rollup.NewWorker(rollup.Config{}, fake, nil)

	mcpSrv := mcpserver.New(mcpserver.Config{
		AuthToken: "integrationtoken",
	}, readsH, worker, nil)

	// Serve via httptest so we get a real HTTP listener.
	ts := httptest.NewServer(mcpSrv.Handler())
	defer ts.Close()

	// Pipe initialize + tools/list + tools/call through the proxy.
	stdin := strings.NewReader(
		rpcLine("initialize") +
			rpcLine("tools/list") +
			rpcLineWithArgs("tools/call", map[string]any{
				"name":      "beacon.errors",
				"arguments": map[string]any{"since": "7d"},
			}),
	)
	var stdout, stderr bytes.Buffer

	env := envMap(map[string]string{"BEACON_AUTH_TOKEN": "integrationtoken"})
	code := cmdMCPProxy([]string{ts.URL + "/rpc"}, stdin, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("exit %d; stderr: %s", code, stderr.String())
	}

	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d response lines, want 3:\n%s", len(lines), stdout.String())
	}

	// Verify initialize response.
	var initResp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initResp); err != nil {
		t.Fatalf("decode initialize: %v", err)
	}
	if initResp.Result.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q", initResp.Result.ProtocolVersion)
	}
	if initResp.Result.ServerInfo.Name != "beacon" {
		t.Errorf("serverInfo.name = %q", initResp.Result.ServerInfo.Name)
	}

	// Verify tools/list has 6 tools.
	var listResp struct {
		Result struct {
			Tools []json.RawMessage `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &listResp); err != nil {
		t.Fatalf("decode tools/list: %v", err)
	}
	if len(listResp.Result.Tools) != 6 {
		t.Errorf("tools count = %d, want 6", len(listResp.Result.Tools))
	}

	// Verify tools/call returned a valid result (not an RPC error).
	var callResp struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[2]), &callResp); err != nil {
		t.Fatalf("decode tools/call: %v", err)
	}
	if callResp.Error != nil {
		t.Fatalf("tools/call returned rpc error: %s", *callResp.Error)
	}
	if len(callResp.Result.Content) == 0 {
		t.Fatal("tools/call returned no content blocks")
	}
	if callResp.Result.Content[0].Type != "text" {
		t.Errorf("content[0].type = %q, want text", callResp.Result.Content[0].Type)
	}
}
