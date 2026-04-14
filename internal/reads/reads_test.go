package reads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
)

var fixedNow = time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)

func newTestHandler(t *testing.T, cfg Config) (*Handler, *memfake.Fake) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	h := NewHandler(cfg, fake, nil)
	h.now = func() time.Time { return fixedNow }
	return h, fake
}

func mux(h *Handler) *http.ServeMux {
	m := http.NewServeMux()
	h.Mount(m)
	return m
}

func mustJSON(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
}

func seedMetric(t *testing.T, fake *memfake.Fake, m beacondb.Metric) {
	t.Helper()
	if err := fake.UpsertMetrics(context.Background(), []beacondb.Metric{m}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
}

func fp(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// GET /api/metrics/{name}
// ---------------------------------------------------------------------------

func TestMetric_foldsHourliesIntoDays(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	// Seed 3 hourly rollups in the same day + 2 in a second day.
	day1 := time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 4, 10, 0, 0, 0, 0, time.UTC)
	for _, h := range []int{10, 11, 12} {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: day1.Add(time.Duration(h) * time.Hour),
			Count:       10,
		})
	}
	for _, h := range []int{9, 10} {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: day2.Add(time.Duration(h) * time.Hour),
			Count:       20,
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/signup.completed?kind=outcome&period_kind=day&window=7d", nil)
	mux(h).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp MetricResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if resp.Kind != "outcome" || resp.Name != "signup.completed" || resp.PeriodKind != "day" {
		t.Errorf("wrong top-level: %+v", resp)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("data len = %d, want 2", len(resp.Data))
	}
	if resp.Data[0].PeriodStart != "2026-04-09" || resp.Data[0].Count != 30 {
		t.Errorf("day 1 = %+v, want 2026-04-09/30", resp.Data[0])
	}
	if resp.Data[1].PeriodStart != "2026-04-10" || resp.Data[1].Count != 40 {
		t.Errorf("day 2 = %+v, want 2026-04-10/40", resp.Data[1])
	}
}

func TestMetric_hourPeriodKindPassthrough(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	hour := fixedNow.Truncate(time.Hour)
	// {name} is a single path segment — use a dotted stand-in for the perf
	// metric's URL key. Slash-bearing perf names (like "GET /dashboard") go
	// through /perf/endpoints in v1; direct lookup would need URL-encoding.
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindPerf, Name: "perf.dashboard",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: hour, Count: 100,
		Sum: fp(15000), P50: fp(100), P95: fp(187), P99: fp(250),
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/perf.dashboard?kind=perf&period_kind=hour&window=6h", nil)
	mux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp MetricResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if resp.PeriodKind != "hour" {
		t.Errorf("period_kind = %q", resp.PeriodKind)
	}
	if len(resp.Data) != 1 {
		t.Fatalf("data len = %d", len(resp.Data))
	}
	if resp.Data[0].P95 == nil || *resp.Data[0].P95 != 187 {
		t.Errorf("p95 = %v", resp.Data[0].P95)
	}
	if !strings.HasPrefix(resp.Data[0].PeriodStart, "2026-04-10T12:00:00") {
		t.Errorf("period_start = %q (expected RFC3339)", resp.Data[0].PeriodStart)
	}
}

func TestMetric_badParams(t *testing.T) {
	h, _ := newTestHandler(t, Config{})
	cases := []struct {
		path string
		want string
	}{
		{"/api/metrics/x?kind=banana", "kind must be outcome"},
		{"/api/metrics/x?kind=outcome&window=bogus", "window:"},
		{"/api/metrics/x?kind=outcome&period_kind=week", "period_kind must be hour or day"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			mux(h).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("code = %d", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("body missing %q: %s", tc.want, rec.Body.String())
			}
		})
	}
}

func TestMetric_baselineSummary(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	// Seed 5 hourly rows with counts [100, 110, 90, 120, 80].
	base := fixedNow.Add(-5 * time.Hour).Truncate(time.Hour)
	counts := []int64{100, 110, 90, 120, 80}
	for i, c := range counts {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       c,
		})
	}
	// Also a baseline row so captured_at comes from it.
	capturedAt := fixedNow.Truncate(time.Hour).Add(-2 * time.Hour)
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindOutcome, Name: "signup.completed",
		PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "30d",
		PeriodStart: capturedAt, Count: 500,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/signup.completed?kind=outcome&period_kind=day&window=7d", nil)
	mux(h).ServeHTTP(rec, req)

	var resp MetricResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if resp.Baseline == nil {
		t.Fatal("baseline missing")
	}
	if resp.Baseline.Window != "30d" {
		t.Errorf("window = %q", resp.Baseline.Window)
	}
	// Mean of [100,110,90,120,80] = 100. Stddev = sqrt(1000/4) = 15.81...
	if resp.Baseline.HourlyCountMean != 100.0 {
		t.Errorf("hourly_count_mean = %v, want 100", resp.Baseline.HourlyCountMean)
	}
	if resp.Baseline.HourlyCountStd < 15.8 || resp.Baseline.HourlyCountStd > 15.9 {
		t.Errorf("hourly_count_stddev = %v, want ~15.81", resp.Baseline.HourlyCountStd)
	}
	if resp.Baseline.CapturedAt != capturedAt.Format(time.RFC3339) {
		t.Errorf("captured_at = %q, want %q", resp.Baseline.CapturedAt, capturedAt.Format(time.RFC3339))
	}
}

func TestMetric_unauthorized(t *testing.T) {
	h, _ := newTestHandler(t, Config{AuthToken: "secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/metrics/x?kind=outcome", nil)
	mux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /api/errors
// ---------------------------------------------------------------------------

func TestErrors_groupsByFingerprint(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	h0 := fixedNow.Add(-3 * time.Hour).Truncate(time.Hour)
	h1 := h0.Add(time.Hour)
	// Two errors: fp-A appears twice (in both hours), fp-B appears once.
	seedMetric(t, fake, beacondb.Metric{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-A", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: h0, Count: 2})
	seedMetric(t, fake, beacondb.Metric{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-A", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: h1, Count: 3})
	seedMetric(t, fake, beacondb.Metric{Kind: beacondb.KindError, Name: "ValueError", Fingerprint: "fp-B", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: h1, Count: 1})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/errors?since=7d", nil)
	mux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp ErrorsResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Errors) != 2 {
		t.Fatalf("errors = %d, want 2", len(resp.Errors))
	}
	byFP := map[string]ErrorSummary{}
	for _, e := range resp.Errors {
		byFP[e.Fingerprint] = e
	}
	if byFP["fp-A"].Occurrences != 5 {
		t.Errorf("fp-A occurrences = %d, want 5", byFP["fp-A"].Occurrences)
	}
	if byFP["fp-A"].FirstSeen != h0.Format(time.RFC3339) {
		t.Errorf("fp-A first_seen = %q, want %q", byFP["fp-A"].FirstSeen, h0.Format(time.RFC3339))
	}
	if byFP["fp-A"].LastSeen != h1.Format(time.RFC3339) {
		t.Errorf("fp-A last_seen = %q, want %q", byFP["fp-A"].LastSeen, h1.Format(time.RFC3339))
	}
	if byFP["fp-B"].Name != "ValueError" {
		t.Errorf("fp-B name = %q", byFP["fp-B"].Name)
	}
}

func TestErrors_newOnly(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	// An "old" error: first seen 10 days ago, still occurring.
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindError, Name: "Old", Fingerprint: "old-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-10 * 24 * time.Hour).Truncate(time.Hour),
		Count:       1,
	})
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindError, Name: "Old", Fingerprint: "old-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-2 * time.Hour).Truncate(time.Hour),
		Count:       1,
	})
	// A "new" error: only seen in the window.
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindError, Name: "Fresh", Fingerprint: "new-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-1 * time.Hour).Truncate(time.Hour),
		Count:       1,
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/errors?since=7d&new_only=true", nil)
	mux(h).ServeHTTP(rec, req)

	var resp ErrorsResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Errors) != 1 {
		t.Fatalf("errors = %d, want 1 (new-fp only)", len(resp.Errors))
	}
	if resp.Errors[0].Fingerprint != "new-fp" {
		t.Errorf("got %q, want new-fp", resp.Errors[0].Fingerprint)
	}
}

// ---------------------------------------------------------------------------
// GET /api/perf/endpoints
// ---------------------------------------------------------------------------

func TestPerfEndpoints_driftOrdering(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	// Two endpoints. Both have stable baselines, then:
	//   /stable — current matches baseline → ~0 drift
	//   /slowed — current p95 is way above baseline mean → big drift
	base := fixedNow.Add(-30 * 24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 100; i++ {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindPerf, Name: "GET /stable",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10, P95: fp(100 + float64(i%3)),
		})
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindPerf, Name: "GET /slowed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10, P95: fp(50),
		})
	}
	// Current window (last 24h): /slowed's p95 jumps to 200.
	currentStart := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindPerf, Name: "GET /slowed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: currentStart.Add(time.Duration(i) * time.Hour),
			Count:       10, P95: fp(200),
		})
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/perf/endpoints?window=24h", nil)
	mux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp PerfResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Endpoints) < 1 {
		t.Fatalf("no endpoints: %+v", resp)
	}
	if resp.Endpoints[0].Name != "GET /slowed" {
		t.Errorf("first (by drift) = %q, want GET /slowed", resp.Endpoints[0].Name)
	}
	if resp.Endpoints[0].DriftSigmas < 5 {
		t.Errorf("drift = %v, expected large positive", resp.Endpoints[0].DriftSigmas)
	}
}

func TestPerfEndpoints_driftFilter(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	// Seed just the /stable endpoint — drift should be near zero.
	base := fixedNow.Add(-10 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 10; i++ {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindPerf, Name: "GET /stable",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10, P95: fp(100),
		})
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/perf/endpoints?window=24h&drift=true", nil)
	mux(h).ServeHTTP(rec, req)

	var resp PerfResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Endpoints) != 0 {
		t.Errorf("drift=true with stable baseline should filter everything, got %+v", resp.Endpoints)
	}
}

// ---------------------------------------------------------------------------
// Helper unit tests
// ---------------------------------------------------------------------------

func TestParseWindow(t *testing.T) {
	cases := []struct {
		in      string
		want    time.Duration
		wantErr bool
	}{
		{"24h", 24 * time.Hour, false},
		{"7d", 7 * 24 * time.Hour, false},
		{"30d", 30 * 24 * time.Hour, false},
		{"15m", 15 * time.Minute, false},
		{"0d", 0, true},
		{"-1h", 0, true},
		{"banana", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseWindow(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Error("expected err")
				}
				return
			}
			if err != nil || got != tc.want {
				t.Errorf("got (%v, %v), want (%v, nil)", got, err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// GetOutcomeSummaries
// ---------------------------------------------------------------------------

func TestOutcomeSummaries_driftOrdering(t *testing.T) {
	h, fake := newTestHandler(t, Config{})

	// Seed two outcome metrics with different baseline profiles.
	// "steady" has a flat baseline matching current → ~0% drift.
	// "spiked" has a low baseline but high recent counts → large drift.
	base := fixedNow.Add(-30 * 24 * time.Hour).Truncate(time.Hour)

	// Baseline period (30d): steady=10/hr, spiked=5/hr
	for i := 0; i < 200; i++ {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "steady",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10,
		})
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "spiked",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       5,
		})
	}

	// Recent window (last 24h): steady stays 10/hr, spiked jumps to 50/hr.
	recent := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "steady",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: recent.Add(time.Duration(i) * time.Hour),
			Count:       10,
		})
		seedMetric(t, fake, beacondb.Metric{
			Kind: beacondb.KindOutcome, Name: "spiked",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: recent.Add(time.Duration(i) * time.Hour),
			Count:       50,
		})
	}

	summaries, err := h.GetOutcomeSummaries(context.Background(), 7*24*time.Hour)
	if err != nil {
		t.Fatalf("GetOutcomeSummaries: %v", err)
	}
	if len(summaries) < 2 {
		t.Fatalf("got %d summaries, want >= 2", len(summaries))
	}

	// "spiked" should be first — it has the largest absolute drift.
	if summaries[0].Name != "spiked" {
		t.Errorf("first summary = %q, want spiked (highest drift)", summaries[0].Name)
	}
	if summaries[0].DriftPercent <= 0 {
		t.Errorf("spiked drift = %.1f%%, want positive", summaries[0].DriftPercent)
	}

	// "steady" should have near-zero drift.
	var steady *OutcomeSummary
	for i := range summaries {
		if summaries[i].Name == "steady" {
			steady = &summaries[i]
			break
		}
	}
	if steady == nil {
		t.Fatal("steady not found in summaries")
	}
	if steady.DriftPercent > 5 || steady.DriftPercent < -5 {
		t.Errorf("steady drift = %.1f%%, want near zero", steady.DriftPercent)
	}

	// HourlyCounts should be populated for sparklines.
	if len(summaries[0].HourlyCounts) == 0 {
		t.Error("spiked HourlyCounts is empty")
	}
}

func TestMeanStddev(t *testing.T) {
	m, s := meanStddev([]float64{10, 20, 30, 40, 50})
	if m != 30 {
		t.Errorf("mean = %v", m)
	}
	// sample stddev of [10,20,30,40,50] = sqrt(1000/4) = ~15.81
	if s < 15.8 || s > 15.9 {
		t.Errorf("stddev = %v, want ~15.81", s)
	}
}

// ---------------------------------------------------------------------------
// Anomalies
// ---------------------------------------------------------------------------

func TestGetAnomalies_returnsSeededAnomalies(t *testing.T) {
	h, fake := newTestHandler(t, Config{})

	// Seed two anomaly records.
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindAmbient, Name: "http_request",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 100, Sum: fp(12.4), P50: fp(10), P95: fp(0.5),
		Fingerprint: "volume_shift", DimensionHash: "",
	})
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindAmbient, Name: "signup.completed",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart:   fixedNow.Add(-2 * time.Hour),
		Count:         47,
		Sum:           fp(8.1),
		P50:           fp(3),
		P95:           fp(1.2),
		Fingerprint:   "dimension_spike",
		Dimensions:    map[string]any{"country": "US"},
		DimensionHash: "abc",
	})

	resp, err := h.GetAnomalies(context.Background(), GetAnomaliesRequest{Since: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Anomalies) != 2 {
		t.Fatalf("anomalies = %d, want 2", len(resp.Anomalies))
	}
	// Sorted by deviation descending: 12.4 first, 8.1 second.
	if resp.Anomalies[0].DeviationSigma != 12.4 {
		t.Errorf("first deviation = %v, want 12.4", resp.Anomalies[0].DeviationSigma)
	}
	if resp.Anomalies[0].AnomalyKind != "volume_shift" {
		t.Errorf("first kind = %q, want volume_shift", resp.Anomalies[0].AnomalyKind)
	}
	if resp.Anomalies[1].AnomalyKind != "dimension_spike" {
		t.Errorf("second kind = %q, want dimension_spike", resp.Anomalies[1].AnomalyKind)
	}
	if resp.Anomalies[1].Dimension["country"] != "US" {
		t.Errorf("second dimension = %+v, want {country: US}", resp.Anomalies[1].Dimension)
	}
}

func TestHandleAnomalies_HTTP(t *testing.T) {
	h, fake := newTestHandler(t, Config{})
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindPerf, Name: "GET /search",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 200, Sum: fp(5.0), P50: fp(20), P95: fp(2),
		Fingerprint: "volume_shift", DimensionHash: "",
	})

	m := mux(h)
	req := httptest.NewRequest(http.MethodGet, "/api/anomalies?since=7d", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp AnomaliesResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Anomalies) != 1 {
		t.Fatalf("anomalies = %d, want 1", len(resp.Anomalies))
	}
	if resp.Anomalies[0].Name != "GET /search" {
		t.Errorf("name = %q, want GET /search", resp.Anomalies[0].Name)
	}
	if resp.Anomalies[0].Current != 200 {
		t.Errorf("current = %d, want 200", resp.Anomalies[0].Current)
	}
}

func TestDismissAnomaly_excludesFromGetAnomalies(t *testing.T) {
	h, fake := newTestHandler(t, Config{})

	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindAmbient, Name: "http_request",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 100, Sum: fp(12.4), P50: fp(10), P95: fp(0.5),
		Fingerprint: "volume_shift",
	})
	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindPerf, Name: "GET /search",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-2 * time.Hour),
		Count: 50, Sum: fp(5.0), P50: fp(10), P95: fp(2),
		Fingerprint: "volume_shift",
	})

	// Both should appear initially.
	resp, err := h.GetAnomalies(context.Background(), GetAnomaliesRequest{Since: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Anomalies) != 2 {
		t.Fatalf("before dismiss: anomalies = %d, want 2", len(resp.Anomalies))
	}

	// Dismiss the first one by ID.
	dismissID := resp.Anomalies[0].ID
	if err := h.DismissAnomaly(context.Background(), dismissID); err != nil {
		t.Fatalf("DismissAnomaly: %v", err)
	}

	// Only one should remain.
	resp, err = h.GetAnomalies(context.Background(), GetAnomaliesRequest{Since: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Anomalies) != 1 {
		t.Fatalf("after dismiss: anomalies = %d, want 1", len(resp.Anomalies))
	}
	if resp.Anomalies[0].ID == dismissID {
		t.Error("dismissed anomaly should not appear in GetAnomalies")
	}
}

func TestDismissAnomaly_notFoundReturnsError(t *testing.T) {
	h, _ := newTestHandler(t, Config{})

	err := h.DismissAnomaly(context.Background(), 99999)
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
	if !errors.Is(err, beacondb.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestDismissAnomaly_alreadyDismissedReturnsError(t *testing.T) {
	h, fake := newTestHandler(t, Config{})

	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindAmbient, Name: "spike",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 100, Sum: fp(5.0), P50: fp(10), P95: fp(1),
		Fingerprint: "volume_shift",
	})

	resp, _ := h.GetAnomalies(context.Background(), GetAnomaliesRequest{Since: 24 * time.Hour})
	id := resp.Anomalies[0].ID

	// First dismiss succeeds.
	if err := h.DismissAnomaly(context.Background(), id); err != nil {
		t.Fatalf("first dismiss: %v", err)
	}
	// Second dismiss returns ErrNotFound (already dismissed).
	err := h.DismissAnomaly(context.Background(), id)
	if !errors.Is(err, beacondb.ErrNotFound) {
		t.Errorf("second dismiss = %v, want ErrNotFound", err)
	}
}

func TestHandleDismissAnomaly_HTTP(t *testing.T) {
	h, fake := newTestHandler(t, Config{})

	seedMetric(t, fake, beacondb.Metric{
		Kind: beacondb.KindPerf, Name: "GET /items",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 200, Sum: fp(5.0), P50: fp(20), P95: fp(2),
		Fingerprint: "volume_shift",
	})

	// Get the ID via the API.
	resp, _ := h.GetAnomalies(context.Background(), GetAnomaliesRequest{Since: 24 * time.Hour})
	id := resp.Anomalies[0].ID

	m := mux(h)

	// DELETE /api/anomalies/:id returns 204.
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/anomalies/%d", id), nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("dismiss status = %d, want 204: %s", rec.Code, rec.Body.String())
	}

	// GET /api/anomalies no longer returns it.
	req = httptest.NewRequest(http.MethodGet, "/api/anomalies?since=24h", nil)
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	var anomResp AnomaliesResponse
	mustJSON(t, rec.Body.Bytes(), &anomResp)
	if len(anomResp.Anomalies) != 0 {
		t.Errorf("anomalies after dismiss = %d, want 0", len(anomResp.Anomalies))
	}

	// DELETE again returns 404 (already dismissed).
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/api/anomalies/%d", id), nil)
	rec = httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("re-dismiss status = %d, want 404", rec.Code)
	}
}

func TestHandleAnomalies_emptyResponse(t *testing.T) {
	h, _ := newTestHandler(t, Config{})
	m := mux(h)
	req := httptest.NewRequest(http.MethodGet, "/api/anomalies", nil)
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp AnomaliesResponse
	mustJSON(t, rec.Body.Bytes(), &resp)
	if len(resp.Anomalies) != 0 {
		t.Errorf("anomalies = %d, want 0", len(resp.Anomalies))
	}
}
