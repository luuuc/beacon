// Package mcpserver exposes Beacon's read path as an MCP (Model Context
// Protocol) handler over JSON-RPC 2.0.
//
// This is a minimal subset of MCP — enough for a client to discover and
// invoke tools — without the full SSE/session machinery the spec defines
// for streaming transports. Each POST carries one JSON-RPC request and
// returns one JSON-RPC response, synchronously.
//
// Supported methods:
//
//	initialize              — version + capabilities handshake
//	notifications/initialized — no-op (returns 200)
//	tools/list              — enumerate the registered tools
//	tools/call              — invoke a tool by name with arguments
//
// Tools delegate to the shared pure-Go query methods on reads.Handler and
// rollup.Worker — the same code paths the HTTP handlers use — so a tool's
// result is guaranteed to match its HTTP sibling byte-for-byte modulo
// JSON field ordering.
//
// The MCP server is read-only in v1. The beacon.deploy_baseline tool is a
// read (returns the most recent deployment baseline row); actual capture
// happens automatically when a deploy.shipped outcome event is ingested.
package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/httputil"
	"github.com/luuuc/beacon/internal/reads"
	"github.com/luuuc/beacon/internal/rollup"
	"github.com/luuuc/beacon/internal/version"
)

// Config holds the settings for the MCP handler. Bind and Port are no
// longer needed — the MCP handler is mounted on the main HTTP server.
type Config struct {
	AuthToken string
}

// Server is an MCP handler provider. It builds HTTP handlers for the MCP
// JSON-RPC endpoint but does not own a listener — the main server mounts
// the handler and owns the socket.
type Server struct {
	cfg     Config
	reads   *reads.Handler
	worker  *rollup.Worker
	log     *slog.Logger
	handler http.Handler
	tools   []toolDef
	toolMap map[string]toolDef
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	handler     toolHandler     `json:"-"`
}

type toolHandler func(ctx context.Context, args json.RawMessage) (any, error)

// New builds the MCP handler. It registers tools but does not own a
// listener — call Handler() to get the http.Handler and mount it on the
// main server's mux.
func New(cfg Config, readsH *reads.Handler, worker *rollup.Worker, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{
		cfg:     cfg,
		reads:   readsH,
		worker:  worker,
		log:     log,
		toolMap: map[string]toolDef{},
	}
	s.registerTools()
	s.handler = http.HandlerFunc(s.handleRPC)
	return s
}

// Handler returns the MCP JSON-RPC handler. The caller mounts it on the
// desired path (e.g. POST /mcp/rpc) and owns the listener.
func (s *Server) Handler() http.Handler { return s.handler }

// ---------------------------------------------------------------------------
// JSON-RPC 2.0 envelope
// ---------------------------------------------------------------------------

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

const (
	errParse          = -32700
	errInvalidRequest = -32600
	errMethodNotFound = -32601
	errInvalidParams  = -32602
	errInternal       = -32603
)

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request) {
	if s.cfg.AuthToken != "" && !httputil.CheckBearer(r.Header.Get("Authorization"), s.cfg.AuthToken) {
		httputil.WriteJSONError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeRPCError(w, nil, errParse, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" {
		s.writeRPCError(w, req.ID, errInvalidRequest, "jsonrpc must be \"2.0\"")
		return
	}

	switch req.Method {
	case "initialize":
		s.writeRPCResult(w, req.ID, s.initializeResult())
	case "notifications/initialized":
		// Notification: no id, no response body required. Return 204.
		w.WriteHeader(http.StatusNoContent)
	case "tools/list":
		s.writeRPCResult(w, req.ID, map[string]any{"tools": s.tools})
	case "tools/call":
		result, rpcErr := s.handleToolsCall(r.Context(), req.Params)
		if rpcErr != nil {
			s.writeRPCErrorStruct(w, req.ID, rpcErr)
			return
		}
		s.writeRPCResult(w, req.ID, result)
	default:
		s.writeRPCError(w, req.ID, errMethodNotFound, "method not found: "+req.Method)
	}
}

func (s *Server) initializeResult() map[string]any {
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "beacon",
			"version": version.Version,
		},
	}
}

// handleToolsCall dispatches to a registered tool and wraps its result in
// the MCP content-block shape. The JSON-stringified payload lives in a
// text content block — the canonical way to return structured data while
// the MCP "structured content" extension is still in flux.
func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &rpcError{Code: errInvalidParams, Message: "invalid params: " + err.Error()}
	}
	tool, ok := s.toolMap[p.Name]
	if !ok {
		return nil, &rpcError{Code: errMethodNotFound, Message: "unknown tool: " + p.Name}
	}
	if len(p.Arguments) == 0 {
		p.Arguments = json.RawMessage("{}")
	}
	result, err := tool.handler(ctx, p.Arguments)
	if err != nil {
		return toolCallError(err.Error()), nil
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return nil, &rpcError{Code: errInternal, Message: "marshal tool result: " + err.Error()}
	}
	return map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": string(payload)},
		},
	}, nil
}

// toolCallError returns an MCP-shaped error result (isError=true) rather
// than a JSON-RPC protocol error. Tool-level failures (bad params, missing
// data) flow through here; protocol-level failures use rpcError.
func toolCallError(msg string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []map[string]any{
			{"type": "text", "text": msg},
		},
	}
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id json.RawMessage, result any) {
	httputil.WriteJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	s.writeRPCErrorStruct(w, id, &rpcError{Code: code, Message: msg})
}

func (s *Server) writeRPCErrorStruct(w http.ResponseWriter, id json.RawMessage, e *rpcError) {
	httputil.WriteJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Error: e})
}

// ---------------------------------------------------------------------------
// Tool registration
// ---------------------------------------------------------------------------

func (s *Server) register(t toolDef) {
	s.tools = append(s.tools, t)
	s.toolMap[t.Name] = t
}

func (s *Server) registerTools() {
	s.register(toolDef{
		Name:        "beacon.metric",
		Description: "Get a single metric over a window with its baseline.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"kind": {"type": "string", "enum": ["outcome","perf","error"]},
				"name": {"type": "string"},
				"window": {"type": "string", "description": "Go duration or Nd shorthand (e.g. 7d). Default 7d."},
				"period_kind": {"type": "string", "enum": ["hour","day"], "default": "day"}
			},
			"required": ["kind","name"]
		}`),
		handler: s.toolMetric,
	})

	s.register(toolDef{
		Name:        "beacon.errors",
		Description: "List error fingerprints active in a window.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"since": {"type": "string", "description": "Window (duration or Nd). Default 7d."},
				"new_only": {"type": "boolean", "description": "Only fingerprints that first appeared in the window."}
			}
		}`),
		handler: s.toolErrors,
	})

	s.register(toolDef{
		Name:        "beacon.perf_drift",
		Description: "List endpoints sorted by performance drift from the 30d baseline.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"window": {"type": "string", "description": "Current window (default 24h)."},
				"drift_only": {"type": "boolean", "description": "Filter to |drift| >= 1 sigma."}
			}
		}`),
		handler: s.toolPerfDrift,
	})

	s.register(toolDef{
		Name:        "beacon.compare",
		Description: "Compare the current window to a captured deployment baseline and return a pass/drift/fail verdict.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"kind": {"type": "string", "enum": ["outcome","perf","error"]},
				"name": {"type": "string"},
				"deploy_time": {"type": "string", "description": "RFC3339 timestamp matching the deploy.shipped event's created_at."}
			},
			"required": ["kind","name","deploy_time"]
		}`),
		handler: s.toolCompare,
	})

	s.register(toolDef{
		Name:        "beacon.outcome_check",
		Description: "Run a pass/drift/fail outcome check for a named event against its most recent deployment baseline.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"name": {"type": "string"},
				"deploy_time": {"type": "string", "description": "RFC3339 timestamp of the deploy."}
			},
			"required": ["name","deploy_time"]
		}`),
		handler: s.toolOutcomeCheck,
	})

	s.register(toolDef{
		Name:        "beacon.deploy_baseline",
		Description: "Return the most recent deployment baseline row for a metric. Read-only: actual capture happens via the deploy.shipped outcome event.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"kind": {"type": "string", "enum": ["outcome","perf","error"]},
				"name": {"type": "string"}
			},
			"required": ["kind","name"]
		}`),
		handler: s.toolDeployBaseline,
	})
}

// ---------------------------------------------------------------------------
// Tool handlers
// ---------------------------------------------------------------------------

func (s *Server) toolMetric(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		Window     string `json:"window"`
		PeriodKind string `json:"period_kind"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	window, err := optionalWindow(p.Window)
	if err != nil {
		return nil, err
	}
	return s.reads.GetMetric(ctx, reads.GetMetricRequest{
		Kind:       beacondb.Kind(p.Kind),
		Name:       p.Name,
		Window:     window,
		PeriodKind: p.PeriodKind,
	})
}

func (s *Server) toolErrors(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Since   string `json:"since"`
		NewOnly bool   `json:"new_only"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	window, err := optionalWindow(p.Since)
	if err != nil {
		return nil, err
	}
	return s.reads.GetErrors(ctx, reads.GetErrorsRequest{
		Since:   window,
		NewOnly: p.NewOnly,
	})
}

func (s *Server) toolPerfDrift(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Window    string `json:"window"`
		DriftOnly bool   `json:"drift_only"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	window, err := optionalWindow(p.Window)
	if err != nil {
		return nil, err
	}
	return s.reads.GetPerfEndpoints(ctx, reads.GetPerfRequest{
		Window:    window,
		DriftOnly: p.DriftOnly,
	})
}

func (s *Server) toolCompare(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		DeployTime string `json:"deploy_time"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	kind := beacondb.Kind(p.Kind)
	if !kind.Valid() {
		return nil, errors.New("kind must be outcome, perf, or error")
	}
	if p.Name == "" {
		return nil, errors.New("name is required")
	}
	deployTime, err := time.Parse(time.RFC3339, p.DeployTime)
	if err != nil {
		return nil, fmt.Errorf("deploy_time: %w", err)
	}
	if s.worker == nil {
		return nil, errors.New("rollup worker not wired")
	}
	return s.worker.CompareDeployBaseline(ctx, kind, p.Name, deployTime.UTC())
}

func (s *Server) toolOutcomeCheck(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Name       string `json:"name"`
		DeployTime string `json:"deploy_time"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if p.Name == "" {
		return nil, errors.New("name is required")
	}
	deployTime, err := time.Parse(time.RFC3339, p.DeployTime)
	if err != nil {
		return nil, fmt.Errorf("deploy_time: %w", err)
	}
	if s.worker == nil {
		return nil, errors.New("rollup worker not wired")
	}
	return s.worker.CompareDeployBaseline(ctx, beacondb.KindOutcome, p.Name, deployTime.UTC())
}

func (s *Server) toolDeployBaseline(ctx context.Context, args json.RawMessage) (any, error) {
	var p struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return s.reads.GetDeployBaseline(ctx, beacondb.Kind(p.Kind), p.Name)
}

// optionalWindow parses a window string, treating empty as "let the callee
// use its default". Returns zero-duration when empty so request structs
// apply their own defaults.
func optionalWindow(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	return reads.ParseWindow(s)
}
