package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/rollup"
)

// fixedNow is the reference clock — same anchor as the reads and rollup
// tests so shared fixtures behave the same under every package.
var fixedNow = time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)

func fp(v float64) *float64 { return &v }

func newTestStack(t *testing.T) (*Server, *memfake.Fake) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock := func() time.Time { return fixedNow }
	readsH := reads.NewHandler(reads.Config{Now: clock}, fake, nil)
	worker := rollup.NewWorker(rollup.Config{Now: clock}, fake, nil)

	srv := New(Config{Bind: "127.0.0.1", Port: 0}, readsH, worker, nil)
	return srv, fake
}

// rpcCall issues one JSON-RPC request via the server's in-process handler
// and returns the decoded response + the raw body for content assertions.
func rpcCall(t *testing.T, srv *Server, method string, params any) (rpcResponse, []byte) {
	t.Helper()
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
	}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	var resp rpcResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return resp, rec.Body.Bytes()
}

// toolResultText pulls the first text block out of a tools/call result and
// unmarshals it into v. Returns the raw text for assertions on shape.
func toolResultText(t *testing.T, resp rpcResponse, v any) string {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	m, ok := resp.Result.(map[string]any)
	if !ok {
		t.Fatalf("result not a map: %+v", resp.Result)
	}
	if isErr, _ := m["isError"].(bool); isErr {
		t.Fatalf("tool returned isError: %+v", m)
	}
	content, _ := m["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("no content blocks: %+v", m)
	}
	first, _ := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if v != nil {
		if err := json.Unmarshal([]byte(text), v); err != nil {
			t.Fatalf("decode tool payload %q: %v", text, err)
		}
	}
	return text
}

// ---------------------------------------------------------------------------
// Health check
// ---------------------------------------------------------------------------

func TestHealthz(t *testing.T) {
	srv, _ := newTestStack(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("status = %q, want ok", body["status"])
	}
}

// ---------------------------------------------------------------------------
// Protocol-level tests
// ---------------------------------------------------------------------------

func TestInitialize(t *testing.T) {
	srv, _ := newTestStack(t)
	resp, _ := rpcCall(t, srv, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "test", "version": "1"},
	})
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	m := resp.Result.(map[string]any)
	if m["protocolVersion"] != "2024-11-05" {
		t.Errorf("protocolVersion = %v", m["protocolVersion"])
	}
	info := m["serverInfo"].(map[string]any)
	if info["name"] != "beacon" {
		t.Errorf("serverInfo.name = %v", info["name"])
	}
}

func TestToolsList(t *testing.T) {
	srv, _ := newTestStack(t)
	resp, _ := rpcCall(t, srv, "tools/list", nil)
	if resp.Error != nil {
		t.Fatalf("rpc error: %+v", resp.Error)
	}
	m := resp.Result.(map[string]any)
	tools := m["tools"].([]any)
	if len(tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(tools))
	}
	want := map[string]bool{
		"beacon.metric": false, "beacon.errors": false, "beacon.perf_drift": false,
		"beacon.compare": false, "beacon.outcome_check": false, "beacon.deploy_baseline": false,
	}
	for _, raw := range tools {
		tool := raw.(map[string]any)
		name := tool["name"].(string)
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool: %s", name)
			continue
		}
		want[name] = true
		if tool["description"] == "" {
			t.Errorf("%s: missing description", name)
		}
		if tool["inputSchema"] == nil {
			t.Errorf("%s: missing inputSchema", name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing tool: %s", name)
		}
	}
}

func TestUnknownMethod(t *testing.T) {
	srv, _ := newTestStack(t)
	resp, _ := rpcCall(t, srv, "nope/what", nil)
	if resp.Error == nil || resp.Error.Code != errMethodNotFound {
		t.Errorf("expected -32601, got %+v", resp.Error)
	}
}

// ---------------------------------------------------------------------------
// Functional equivalence: each tool's result matches the HTTP sibling.
// ---------------------------------------------------------------------------

func TestToolMetric_matchesHTTPSibling(t *testing.T) {
	srv, fake := newTestStack(t)
	hour := fixedNow.Truncate(time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: hour, Count: 42},
	})

	// HTTP sibling
	http, _ := srv.reads.GetMetric(context.Background(), reads.GetMetricRequest{
		Kind: beacondb.KindOutcome, Name: "signup.completed",
		Window: 24 * time.Hour, PeriodKind: "day",
	})

	// MCP tool
	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name": "beacon.metric",
		"arguments": map[string]any{
			"kind":        "outcome",
			"name":        "signup.completed",
			"window":      "24h",
			"period_kind": "day",
		},
	})
	var viaMCP reads.MetricResponse
	toolResultText(t, resp, &viaMCP)

	// Compare by re-marshaling both to JSON.
	a, _ := json.Marshal(http)
	b, _ := json.Marshal(&viaMCP)
	if string(a) != string(b) {
		t.Errorf("MCP != HTTP:\n  http: %s\n  mcp:  %s", a, b)
	}
}

func TestToolErrors_matchesHTTPSibling(t *testing.T) {
	srv, fake := newTestStack(t)
	hour := fixedNow.Add(-2 * time.Hour).Truncate(time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-A",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: hour, Count: 3},
	})

	http, _ := srv.reads.GetErrors(context.Background(), reads.GetErrorsRequest{Since: 7 * 24 * time.Hour})

	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name":      "beacon.errors",
		"arguments": map[string]any{"since": "7d"},
	})
	var viaMCP reads.ErrorsResponse
	toolResultText(t, resp, &viaMCP)

	a, _ := json.Marshal(http)
	b, _ := json.Marshal(&viaMCP)
	if string(a) != string(b) {
		t.Errorf("MCP != HTTP:\n  http: %s\n  mcp:  %s", a, b)
	}
}

func TestToolPerfDrift_matchesHTTPSibling(t *testing.T) {
	srv, fake := newTestStack(t)
	hour := fixedNow.Add(-3 * time.Hour).Truncate(time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindPerf, Name: "api.home",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: hour, Count: 10, P95: fp(150)},
	})

	http, _ := srv.reads.GetPerfEndpoints(context.Background(), reads.GetPerfRequest{Window: 24 * time.Hour})

	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name":      "beacon.perf_drift",
		"arguments": map[string]any{"window": "24h"},
	})
	var viaMCP reads.PerfResponse
	toolResultText(t, resp, &viaMCP)

	a, _ := json.Marshal(http)
	b, _ := json.Marshal(&viaMCP)
	if string(a) != string(b) {
		t.Errorf("MCP != HTTP:\n  http: %s\n  mcp:  %s", a, b)
	}
}

func TestToolDeployBaseline_returnsCapturedRow(t *testing.T) {
	srv, fake := newTestStack(t)
	deployTime := fixedNow.Add(-3 * time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "deploy",
			PeriodStart: deployTime, Count: 500},
	})

	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name":      "beacon.deploy_baseline",
		"arguments": map[string]any{"kind": "outcome", "name": "signup.completed"},
	})
	var viaMCP reads.DeployBaselineResponse
	toolResultText(t, resp, &viaMCP)
	if viaMCP.Count != 500 {
		t.Errorf("count = %d, want 500", viaMCP.Count)
	}
	if viaMCP.CapturedAt != deployTime.Format(time.RFC3339) {
		t.Errorf("captured_at = %q, want %q", viaMCP.CapturedAt, deployTime.Format(time.RFC3339))
	}
}

func TestToolCompare_usesWorker(t *testing.T) {
	srv, fake := newTestStack(t)
	deployTime := fixedNow.Add(-1 * time.Hour).Truncate(time.Hour)
	// Baseline row: 100 pre-deploy.
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "deploy",
			PeriodStart: deployTime, Count: 100},
	})
	// Hourly row after deploy: 95 events in the current hour.
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: deployTime, Count: 95},
	})

	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name": "beacon.compare",
		"arguments": map[string]any{
			"kind":        "outcome",
			"name":        "signup.completed",
			"deploy_time": deployTime.Format(time.RFC3339),
		},
	})
	var cmp rollup.Comparison
	toolResultText(t, resp, &cmp)
	if cmp.Baseline != 100 {
		t.Errorf("baseline = %d, want 100", cmp.Baseline)
	}
	// Current = 95 scaled to 24h: 95 * 24 / 1 = 2280. That's fail.
	if cmp.Verdict == "" {
		t.Error("verdict missing")
	}
}

func TestToolOutcomeCheck_forcesOutcomeKind(t *testing.T) {
	srv, fake := newTestStack(t)
	deployTime := fixedNow.Add(-1 * time.Hour).Truncate(time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "checkout.succeeded",
			PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "deploy",
			PeriodStart: deployTime, Count: 50},
	})
	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name": "beacon.outcome_check",
		"arguments": map[string]any{
			"name":        "checkout.succeeded",
			"deploy_time": deployTime.Format(time.RFC3339),
		},
	})
	var cmp rollup.Comparison
	toolResultText(t, resp, &cmp)
	if cmp.Baseline != 50 {
		t.Errorf("baseline = %d, want 50", cmp.Baseline)
	}
}

// ---------------------------------------------------------------------------
// Error paths
// ---------------------------------------------------------------------------

func TestToolsCall_unknownTool(t *testing.T) {
	srv, _ := newTestStack(t)
	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name": "beacon.nope", "arguments": map[string]any{},
	})
	if resp.Error == nil || resp.Error.Code != errMethodNotFound {
		t.Errorf("expected -32601, got %+v", resp.Error)
	}
}

func TestToolsCall_validationFailureReturnsIsError(t *testing.T) {
	srv, _ := newTestStack(t)
	resp, _ := rpcCall(t, srv, "tools/call", map[string]any{
		"name": "beacon.metric",
		"arguments": map[string]any{
			"kind": "banana",
			"name": "x",
		},
	})
	if resp.Error != nil {
		t.Fatalf("expected tool-level error, got rpc error: %+v", resp.Error)
	}
	m := resp.Result.(map[string]any)
	if isErr, _ := m["isError"].(bool); !isErr {
		t.Errorf("isError not set: %+v", m)
	}
}

func TestAuthRequired(t *testing.T) {
	fake := memfake.New()
	_ = fake.Migrate(context.Background())
	readsH := reads.NewHandler(reads.Config{}, fake, nil)
	worker := rollup.NewWorker(rollup.Config{}, fake, nil)
	srv := New(Config{AuthToken: "secret"}, readsH, worker, nil)

	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	req := httptest.NewRequest(http.MethodPost, "/rpc", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d", rec.Code)
	}
}
