package reads

import (
	"context"
	"encoding/json"
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
// GET /metrics/{name}
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
	req := httptest.NewRequest(http.MethodGet, "/metrics/signup.completed?kind=outcome&period_kind=day&window=7d", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/metrics/perf.dashboard?kind=perf&period_kind=hour&window=6h", nil)
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
		{"/metrics/x?kind=banana", "kind must be outcome"},
		{"/metrics/x?kind=outcome&window=bogus", "window:"},
		{"/metrics/x?kind=outcome&period_kind=week", "period_kind must be hour or day"},
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
	req := httptest.NewRequest(http.MethodGet, "/metrics/signup.completed?kind=outcome&period_kind=day&window=7d", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/metrics/x?kind=outcome", nil)
	mux(h).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// GET /errors
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
	req := httptest.NewRequest(http.MethodGet, "/errors?since=7d", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/errors?since=7d&new_only=true", nil)
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
// GET /perf/endpoints
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
	req := httptest.NewRequest(http.MethodGet, "/perf/endpoints?window=24h", nil)
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
	req := httptest.NewRequest(http.MethodGet, "/perf/endpoints?window=24h&drift=true", nil)
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
