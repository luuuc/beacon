// Package reads is Beacon's HTTP read path and the pure-Go query layer that
// both HTTP handlers and the MCP server call. Everything here is read-only.
//
// Layering:
//
//   - GetMetric / GetErrors / GetPerfEndpoints / GetDeployBaseline are the
//     pure-Go query methods. They take typed request structs, return typed
//     response structs, and never touch net/http. The MCP server in
//     internal/mcpserver calls these directly.
//   - handleMetric / handleErrors / handlePerfEndpoints parse HTTP query
//     params into the typed requests, call the query methods, and marshal
//     the response with httputil.WriteJSON.
//   - The response types are exported (PascalCase) so the MCP server can
//     return them in its tool-call results without type surgery.
//
// v1 simplifications:
//
//   - GetMetric supports period_kind=day by aggregating hourly rollup rows
//     in Go at read time. Dedicated day-level storage is a future card.
//   - BaselineSummary mean/stddev come from the hourly rows in the baseline
//     window, not from stored stats fields. The baseline row contributes
//     captured_at + the window label.
//   - {name} is a single path segment in the HTTP layer. Slash-bearing perf
//     names go through GetPerfEndpoints rather than direct lookup.
package reads

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/httputil"
)

type Config struct {
	AuthToken string
}

type Handler struct {
	cfg     Config
	adapter beacondb.Adapter
	log     *slog.Logger
	now     func() time.Time
}

func NewHandler(cfg Config, adapter beacondb.Adapter, log *slog.Logger) *Handler {
	if log == nil {
		log = slog.Default()
	}
	return &Handler{cfg: cfg, adapter: adapter, log: log, now: time.Now}
}

// SetNow overrides the handler's clock. Test-only.
func (h *Handler) SetNow(now func() time.Time) { h.now = now }

// Mount registers the three HTTP read routes. The MCP server registers its
// own transport separately and calls the Get* methods below directly.
func (h *Handler) Mount(mux interface {
	Handle(pattern string, handler http.Handler)
}) {
	mux.Handle("GET /metrics/{name}", httputil.BearerMiddleware(h.cfg.AuthToken, http.HandlerFunc(h.handleMetric)))
	mux.Handle("GET /errors", httputil.BearerMiddleware(h.cfg.AuthToken, http.HandlerFunc(h.handleErrors)))
	mux.Handle("GET /perf/endpoints", httputil.BearerMiddleware(h.cfg.AuthToken, http.HandlerFunc(h.handlePerfEndpoints)))
}

// ---------------------------------------------------------------------------
// Response types (exported for MCP reuse)
// ---------------------------------------------------------------------------

type MetricResponse struct {
	Kind       string           `json:"kind"`
	Name       string           `json:"name"`
	PeriodKind string           `json:"period_kind"`
	Data       []MetricPoint    `json:"data"`
	Baseline   *BaselineSummary `json:"baseline,omitempty"`
}

type MetricPoint struct {
	PeriodStart string   `json:"period_start"`
	Count       int64    `json:"count"`
	P50         *float64 `json:"p50,omitempty"`
	P95         *float64 `json:"p95,omitempty"`
	P99         *float64 `json:"p99,omitempty"`
}

type BaselineSummary struct {
	Window     string  `json:"window"`
	Mean       float64 `json:"mean"`
	Stddev     float64 `json:"stddev"`
	CapturedAt string  `json:"captured_at"`
}

type ErrorsResponse struct {
	Errors []ErrorSummary `json:"errors"`
}

type ErrorSummary struct {
	Name        string `json:"name"`
	Fingerprint string `json:"fingerprint"`
	FirstSeen   string `json:"first_seen"`
	LastSeen    string `json:"last_seen"`
	Occurrences int64  `json:"occurrences"`
}

type PerfResponse struct {
	Endpoints []PerfEndpoint `json:"endpoints"`
}

type PerfEndpoint struct {
	Name           string  `json:"name"`
	CurrentP95     float64 `json:"current_p95"`
	BaselineP95    float64 `json:"baseline_p95"`
	BaselineStddev float64 `json:"baseline_stddev"`
	DriftSigmas    float64 `json:"drift_sigmas"`
}

type DeployBaselineResponse struct {
	Kind       string   `json:"kind"`
	Name       string   `json:"name"`
	CapturedAt string   `json:"captured_at"`
	Count      int64    `json:"count"`
	Sum        *float64 `json:"sum,omitempty"`
	P50        *float64 `json:"p50,omitempty"`
	P95        *float64 `json:"p95,omitempty"`
	P99        *float64 `json:"p99,omitempty"`
}

// ---------------------------------------------------------------------------
// Request types
// ---------------------------------------------------------------------------

type GetMetricRequest struct {
	Kind       beacondb.Kind
	Name       string
	Window     time.Duration // defaults to 7d when zero
	PeriodKind string        // "hour" or "day"; defaults to "day" when empty
}

type GetErrorsRequest struct {
	Since   time.Duration // defaults to 7d
	NewOnly bool
}

type GetPerfRequest struct {
	Window    time.Duration // defaults to 24h
	DriftOnly bool
}

// ---------------------------------------------------------------------------
// GetMetric
// ---------------------------------------------------------------------------

// GetMetric is the shared query path for GET /metrics/{name} and the MCP
// beacon.metric tool.
func (h *Handler) GetMetric(ctx context.Context, req GetMetricRequest) (*MetricResponse, error) {
	if !req.Kind.Valid() {
		return nil, errors.New("kind must be outcome, perf, or error")
	}
	if req.Name == "" {
		return nil, errors.New("name is required")
	}
	if req.Window == 0 {
		req.Window = 7 * 24 * time.Hour
	}
	if req.PeriodKind == "" {
		req.PeriodKind = "day"
	}
	if req.PeriodKind != "hour" && req.PeriodKind != "day" {
		return nil, errors.New("period_kind must be hour or day")
	}

	now := h.now().UTC()
	since := now.Add(-req.Window)

	hourlies, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:       req.Kind,
		Name:       req.Name,
		PeriodKind: beacondb.PeriodHour,
		Since:      since,
		Until:      now.Add(time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("list hourlies: %w", err)
	}

	var points []MetricPoint
	if req.PeriodKind == "hour" {
		for _, m := range hourlies {
			points = append(points, hourlyToPoint(m))
		}
	} else {
		points = foldHourliesIntoDays(hourlies)
	}

	baseline, _ := h.buildBaselineSummary(ctx, req.Kind, req.Name, now)

	return &MetricResponse{
		Kind:       string(req.Kind),
		Name:       req.Name,
		PeriodKind: req.PeriodKind,
		Data:       points,
		Baseline:   baseline,
	}, nil
}

func (h *Handler) handleMetric(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	kind := beacondb.Kind(r.URL.Query().Get("kind"))
	windowStr := strOr(r.URL.Query().Get("window"), "7d")
	window, err := parseWindow(windowStr)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "window: "+err.Error())
		return
	}
	periodKind := strOr(r.URL.Query().Get("period_kind"), "day")
	if periodKind != "hour" && periodKind != "day" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "period_kind must be hour or day")
		return
	}

	resp, err := h.GetMetric(r.Context(), GetMetricRequest{
		Kind:       kind,
		Name:       name,
		Window:     window,
		PeriodKind: periodKind,
	})
	if err != nil {
		h.writeQueryError(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

func hourlyToPoint(m beacondb.Metric) MetricPoint {
	return MetricPoint{
		PeriodStart: m.PeriodStart.UTC().Format(time.RFC3339),
		Count:       m.Count,
		P50:         m.P50,
		P95:         m.P95,
		P99:         m.P99,
	}
}

func foldHourliesIntoDays(hourlies []beacondb.Metric) []MetricPoint {
	type dayAcc struct {
		count                  int64
		pSum50, pSum95, pSum99 float64
		pCnt50, pCnt95, pCnt99 int64
	}
	byDay := map[string]*dayAcc{}
	order := []string{}
	for _, m := range hourlies {
		day := m.PeriodStart.UTC().Format("2006-01-02")
		a, ok := byDay[day]
		if !ok {
			a = &dayAcc{}
			byDay[day] = a
			order = append(order, day)
		}
		a.count += m.Count
		if m.P50 != nil {
			a.pSum50 += *m.P50 * float64(m.Count)
			a.pCnt50 += m.Count
		}
		if m.P95 != nil {
			a.pSum95 += *m.P95 * float64(m.Count)
			a.pCnt95 += m.Count
		}
		if m.P99 != nil {
			a.pSum99 += *m.P99 * float64(m.Count)
			a.pCnt99 += m.Count
		}
	}
	sort.Strings(order)
	out := make([]MetricPoint, 0, len(order))
	for _, day := range order {
		a := byDay[day]
		pt := MetricPoint{PeriodStart: day, Count: a.count}
		if a.pCnt50 > 0 {
			v := a.pSum50 / float64(a.pCnt50)
			pt.P50 = &v
		}
		if a.pCnt95 > 0 {
			v := a.pSum95 / float64(a.pCnt95)
			pt.P95 = &v
		}
		if a.pCnt99 > 0 {
			v := a.pSum99 / float64(a.pCnt99)
			pt.P99 = &v
		}
		out = append(out, pt)
	}
	return out
}

func (h *Handler) buildBaselineSummary(ctx context.Context, kind beacondb.Kind, name string, now time.Time) (*BaselineSummary, error) {
	const baselineLabel = "30d"
	baselineWindow := 30 * 24 * time.Hour

	hourly, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:       kind,
		Name:       name,
		PeriodKind: beacondb.PeriodHour,
		Since:      now.Add(-baselineWindow),
		Until:      now,
	})
	if err != nil {
		return nil, err
	}
	if len(hourly) == 0 {
		return nil, nil
	}
	counts := make([]float64, 0, len(hourly))
	for _, m := range hourly {
		counts = append(counts, float64(m.Count))
	}
	mean, stddev := meanStddev(counts)

	capturedAt := now.Truncate(time.Hour).Format(time.RFC3339)
	baselines, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:         kind,
		Name:         name,
		PeriodKind:   beacondb.PeriodBaseline,
		PeriodWindow: baselineLabel,
	})
	if err == nil {
		var latest *beacondb.Metric
		for i := range baselines {
			if latest == nil || baselines[i].PeriodStart.After(latest.PeriodStart) {
				latest = &baselines[i]
			}
		}
		if latest != nil {
			capturedAt = latest.PeriodStart.UTC().Format(time.RFC3339)
		}
	}

	return &BaselineSummary{
		Window:     baselineLabel,
		Mean:       roundTo(mean, 2),
		Stddev:     roundTo(stddev, 2),
		CapturedAt: capturedAt,
	}, nil
}

// ---------------------------------------------------------------------------
// GetErrors
// ---------------------------------------------------------------------------

// GetErrors is the shared query path for GET /errors and the MCP
// beacon.errors tool.
func (h *Handler) GetErrors(ctx context.Context, req GetErrorsRequest) (*ErrorsResponse, error) {
	if req.Since == 0 {
		req.Since = 7 * 24 * time.Hour
	}
	now := h.now().UTC()
	since := now.Add(-req.Since)

	hourlies, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:       beacondb.KindError,
		PeriodKind: beacondb.PeriodHour,
		Since:      since,
		Until:      now.Add(time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("list error metrics: %w", err)
	}

	type groupKey struct {
		name, fingerprint string
	}
	type acc struct {
		name, fingerprint string
		first, last       time.Time
		occurrences       int64
	}
	groups := map[groupKey]*acc{}
	for _, m := range hourlies {
		gk := groupKey{name: m.Name, fingerprint: m.Fingerprint}
		a, ok := groups[gk]
		if !ok {
			a = &acc{name: m.Name, fingerprint: m.Fingerprint, first: m.PeriodStart, last: m.PeriodStart}
			groups[gk] = a
		}
		if m.PeriodStart.Before(a.first) {
			a.first = m.PeriodStart
		}
		if m.PeriodStart.After(a.last) {
			a.last = m.PeriodStart
		}
		a.occurrences += m.Count
	}

	if req.NewOnly {
		filtered := map[groupKey]*acc{}
		for gk, a := range groups {
			prior, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
				Kind:        beacondb.KindError,
				Name:        gk.name,
				Fingerprint: gk.fingerprint,
				PeriodKind:  beacondb.PeriodHour,
				Until:       since,
				Limit:       1,
			})
			if err != nil {
				return nil, fmt.Errorf("new_only prior query: %w", err)
			}
			if len(prior) == 0 {
				filtered[gk] = a
			}
		}
		groups = filtered
	}

	out := make([]ErrorSummary, 0, len(groups))
	for _, a := range groups {
		out = append(out, ErrorSummary{
			Name:        a.name,
			Fingerprint: a.fingerprint,
			FirstSeen:   a.first.UTC().Format(time.RFC3339),
			LastSeen:    a.last.UTC().Format(time.RFC3339),
			Occurrences: a.occurrences,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen > out[j].FirstSeen })
	return &ErrorsResponse{Errors: out}, nil
}

func (h *Handler) handleErrors(w http.ResponseWriter, r *http.Request) {
	window, err := parseWindow(strOr(r.URL.Query().Get("since"), "7d"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	resp, err := h.GetErrors(r.Context(), GetErrorsRequest{
		Since:   window,
		NewOnly: r.URL.Query().Get("new_only") == "true",
	})
	if err != nil {
		h.writeQueryError(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GetPerfEndpoints
// ---------------------------------------------------------------------------

// GetPerfEndpoints is the shared query path for GET /perf/endpoints and the
// MCP beacon.perf_drift tool.
func (h *Handler) GetPerfEndpoints(ctx context.Context, req GetPerfRequest) (*PerfResponse, error) {
	if req.Window == 0 {
		req.Window = 24 * time.Hour
	}
	now := h.now().UTC()
	baselineWindow := 30 * 24 * time.Hour
	currentCutoff := now.Add(-req.Window)

	allRows, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:       beacondb.KindPerf,
		PeriodKind: beacondb.PeriodHour,
		Since:      currentCutoff.Add(-baselineWindow),
		Until:      now.Add(time.Hour),
	})
	if err != nil {
		return nil, fmt.Errorf("list perf metrics: %w", err)
	}
	if len(allRows) == 0 {
		return &PerfResponse{Endpoints: []PerfEndpoint{}}, nil
	}

	type nameAcc struct {
		current, baseline []float64
	}
	byName := map[string]*nameAcc{}
	for _, m := range allRows {
		if m.P95 == nil {
			continue
		}
		a, ok := byName[m.Name]
		if !ok {
			a = &nameAcc{}
			byName[m.Name] = a
		}
		if m.PeriodStart.Before(currentCutoff) {
			a.baseline = append(a.baseline, *m.P95)
		} else {
			a.current = append(a.current, *m.P95)
		}
	}

	out := make([]PerfEndpoint, 0, len(byName))
	for name, a := range byName {
		if len(a.current) == 0 {
			continue
		}
		curMean := mean(a.current)
		bMean, bStddev := meanStddev(a.baseline)
		var drift float64
		if bStddev > 0 {
			drift = (curMean - bMean) / bStddev
		}
		if req.DriftOnly && math.Abs(drift) < 1 {
			continue
		}
		out = append(out, PerfEndpoint{
			Name:           name,
			CurrentP95:     roundTo(curMean, 2),
			BaselineP95:    roundTo(bMean, 2),
			BaselineStddev: roundTo(bStddev, 2),
			DriftSigmas:    roundTo(drift, 2),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return math.Abs(out[i].DriftSigmas) > math.Abs(out[j].DriftSigmas)
	})
	return &PerfResponse{Endpoints: out}, nil
}

func (h *Handler) handlePerfEndpoints(w http.ResponseWriter, r *http.Request) {
	window, err := parseWindow(strOr(r.URL.Query().Get("window"), "24h"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "window: "+err.Error())
		return
	}
	resp, err := h.GetPerfEndpoints(r.Context(), GetPerfRequest{
		Window:    window,
		DriftOnly: r.URL.Query().Get("drift") == "true",
	})
	if err != nil {
		h.writeQueryError(w, err)
		return
	}
	httputil.WriteJSON(w, http.StatusOK, resp)
}

// ---------------------------------------------------------------------------
// GetDeployBaseline (MCP only for v1 — no HTTP endpoint)
// ---------------------------------------------------------------------------

// GetDeployBaseline returns the most recent deployment baseline row for
// (kind, name). Used by the MCP beacon.deploy_baseline tool. In v1 this is
// read-only — actual deploy-baseline capture happens automatically when a
// deploy.shipped outcome event is ingested.
func (h *Handler) GetDeployBaseline(ctx context.Context, kind beacondb.Kind, name string) (*DeployBaselineResponse, error) {
	if !kind.Valid() {
		return nil, errors.New("kind must be outcome, perf, or error")
	}
	if name == "" {
		return nil, errors.New("name is required")
	}
	rows, err := h.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:         kind,
		Name:         name,
		PeriodKind:   beacondb.PeriodBaseline,
		PeriodWindow: "deploy",
	})
	if err != nil {
		return nil, fmt.Errorf("list deploy baselines: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}
	latest := rows[0]
	for i := range rows {
		if rows[i].PeriodStart.After(latest.PeriodStart) {
			latest = rows[i]
		}
	}
	return &DeployBaselineResponse{
		Kind:       string(latest.Kind),
		Name:       latest.Name,
		CapturedAt: latest.PeriodStart.UTC().Format(time.RFC3339),
		Count:      latest.Count,
		Sum:        latest.Sum,
		P50:        latest.P50,
		P95:        latest.P95,
		P99:        latest.P99,
	}, nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func (h *Handler) writeQueryError(w http.ResponseWriter, err error) {
	// Validation errors from the Get* methods bubble up as plain errors.
	// Map them to 400; anything else is a storage failure → 503.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "kind must be"),
		strings.Contains(msg, "name is required"),
		strings.Contains(msg, "period_kind must be"):
		httputil.WriteJSONError(w, http.StatusBadRequest, msg)
	default:
		h.log.Error("reads: query error", "err", err)
		httputil.WriteJSONError(w, http.StatusServiceUnavailable, "storage error")
	}
}

func parseWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, "d") {
		n := 0
		if _, err := fmt.Sscanf(s, "%dd", &n); err != nil {
			return 0, err
		}
		if n <= 0 {
			return 0, fmt.Errorf("must be positive")
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return d, nil
}

// ParseWindow is exported for use by mcpserver when it parses tool arguments.
func ParseWindow(s string) (time.Duration, error) { return parseWindow(s) }

func strOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var s float64
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

func meanStddev(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	m := mean(xs)
	if len(xs) == 1 {
		return m, 0
	}
	var sumSq float64
	for _, x := range xs {
		sumSq += (x - m) * (x - m)
	}
	return m, math.Sqrt(sumSq / float64(len(xs)-1))
}

func roundTo(f float64, digits int) float64 {
	scale := math.Pow(10, float64(digits))
	return math.Round(f*scale) / scale
}
