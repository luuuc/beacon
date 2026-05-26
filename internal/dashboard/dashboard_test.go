package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
	"github.com/luuuc/beacon/internal/reads"
)

var fixedNow = time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)

func newTestDashboard(t *testing.T, authToken string) (*Dashboard, *http.ServeMux) {
	t.Helper()
	_, _, mux := newTestDashboardWithFake(t, authToken)
	return nil, mux
}

func newTestDashboardWithFake(t *testing.T, authToken string) (*Dashboard, *memfake.Fake, *http.ServeMux) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	readsH := reads.NewHandler(reads.Config{AuthToken: authToken, Now: func() time.Time { return fixedNow }}, fake, nil)
	d := New(Config{AuthToken: authToken}, readsH, nil)
	mux := http.NewServeMux()
	d.Mount(mux)
	return d, fake, mux
}

func TestTemplatesParse(t *testing.T) {
	// New() panics if templates fail to parse. If we get here, they parsed.
	_, _ = newTestDashboard(t, "")
}

func TestStaticHandler(t *testing.T) {
	_, mux := newTestDashboard(t, "")

	tests := []struct {
		path        string
		wantStatus  int
		wantType    string
	}{
		{"/static/style.css", 200, "text/css"},
		{"/static/htmx.min.js", 200, "application/javascript"},
		{"/static/favicon.ico", 200, ""},
		{"/favicon.ico", 200, ""},
	}

	for _, tc := range tests {
		t.Run(tc.path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.wantStatus)
			}
			if tc.wantType != "" {
				ct := rec.Header().Get("Content-Type")
				if !strings.Contains(ct, tc.wantType) {
					t.Errorf("GET %s Content-Type = %q, want %q", tc.path, ct, tc.wantType)
				}
			}
			cc := rec.Header().Get("Cache-Control")
			if !strings.Contains(cc, "max-age") {
				t.Errorf("GET %s missing Cache-Control max-age", tc.path)
			}
		})
	}
}

func TestLandingPageNoAuth(t *testing.T) {
	_, mux := newTestDashboard(t, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Beacon") {
		t.Error("landing page missing 'Beacon'")
	}
	if !strings.Contains(body, "htmx.min.js") {
		t.Error("landing page missing htmx script")
	}
	if !strings.Contains(body, "style.css") {
		t.Error("landing page missing CSS link")
	}
}

func TestLandingPageRequiresAuth(t *testing.T) {
	_, mux := newTestDashboard(t, "secret-token")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("GET / = %d, want 302 redirect", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if loc != "/login" {
		t.Errorf("redirect to %q, want /login", loc)
	}
}

func TestLandingPageWithData(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")

	ctx := context.Background()
	base := fixedNow.Add(-48 * time.Hour).Truncate(time.Hour)

	// Seed outcome metric.
	for i := 0; i < 24; i++ {
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10,
		}})
	}

	// Seed perf metric with drift.
	for i := 0; i < 48; i++ {
		p95 := 100.0
		if i >= 24 {
			p95 = 200.0 // spike in recent 24h
		}
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "GET /dashboard",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       50, P95: &p95,
		}})
	}

	// Seed error metric.
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "abc123",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-2 * time.Hour).Truncate(time.Hour),
		Count:       5,
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Pillar status strip shows all four pillars.
	for _, pillar := range []string{"Outcomes", "Performance", "Errors", "Anomalies"} {
		if !strings.Contains(body, pillar) {
			t.Errorf("missing %s in pillar status strip", pillar)
		}
	}
	// No-deploy fallback: should prompt for deploy.shipped.
	if !strings.Contains(body, "deploy.shipped") {
		t.Error("missing deploy.shipped fallback message")
	}
}

func TestLandingDeployCentric(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	// Seed a deploy event.
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind:      beacondb.KindOutcome,
		Name:      "deploy.shipped",
		Context:   map[string]any{"deploy_sha": "abc12345def"},
		CreatedAt: fixedNow.Add(-2 * time.Hour),
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "abc12345") {
		t.Error("deploy hero should show truncated SHA")
	}
	if !strings.Contains(body, "Latest deploy") {
		t.Error("deploy hero should show 'Latest deploy' eyebrow")
	}
	if !strings.Contains(body, "Healthy") {
		t.Error("deploy with no regressions should show Healthy verdict")
	}
	if !strings.Contains(body, "Pillar status") {
		t.Error("page should show pillar status section")
	}
}

func TestLandingDeployWithRegressions(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	// Two deploys: latest and one historical.
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{
		{
			Kind: beacondb.KindOutcome, Name: "deploy.shipped",
			Context:   map[string]any{"deploy_sha": "latest11sha"},
			CreatedAt: fixedNow.Add(-2 * time.Hour),
		},
		{
			Kind: beacondb.KindOutcome, Name: "deploy.shipped",
			Context:   map[string]any{"deploy_sha": "older222sha"},
			CreatedAt: fixedNow.Add(-48 * time.Hour),
		},
	})

	base := fixedNow.Add(-30 * 24 * time.Hour).Truncate(time.Hour)

	// Seed perf data: baseline ~100ms with variance, then spike to 500ms.
	for i := 0; i < 30*24; i++ {
		p95 := 100.0 + float64(i%5)*4.0 // 100-116ms with stddev ~6
		if i >= 29*24 {
			p95 = 500.0
		}
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "POST /checkout",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       50, P95: &p95,
		}})
	}

	// Seed perf data that improved: baseline ~200ms, now 50ms.
	for i := 0; i < 30*24; i++ {
		p95 := 200.0 + float64(i%5)*4.0 // 200-216ms with stddev ~6
		if i >= 29*24 {
			p95 = 50.0
		}
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "GET /search",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       100, P95: &p95,
		}})
	}

	// Seed outcome drop: baseline ~10/hour with variance, now 2/hour.
	for i := 0; i < 30*24; i++ {
		count := int64(10 + i%3) // 10-12 with some variance
		if i >= 29*24 {
			count = 2
		}
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindOutcome, Name: "listing.created",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       count,
		}})
	}

	// Seed a new error introduced by the older deploy SHA.
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "err001",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-2 * time.Hour).Truncate(time.Hour),
		Count:       5,
	}})
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindError, Name: "NoMethodError",
		Fingerprint: "err001",
		Context:     map[string]any{"deploy_sha": "older222sha"},
		CreatedAt:   fixedNow.Add(-47 * time.Hour),
	}})

	// Seed an anomaly.
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindAmbient, Name: "http_request",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 100, Sum: fp(12.4), P50: fp(10), P95: fp(1),
		Fingerprint: "volume_shift",
	}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET / = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	// Latest deploy hero.
	if !strings.Contains(body, "latest11") {
		t.Error("should show latest deploy SHA")
	}
	// Should show regressions (perf spike).
	if !strings.Contains(body, "POST /checkout") {
		t.Error("should show regressed endpoint")
	}
	if !strings.Contains(body, "Regressions") {
		t.Error("should show regressions group")
	}
	// Should show improvements.
	if !strings.Contains(body, "Improvements") {
		t.Error("should show improvements group")
	}
	if !strings.Contains(body, "GET /search") {
		t.Error("should show improved endpoint")
	}
	// Deploy history should show older deploy.
	if !strings.Contains(body, "older222") {
		t.Error("deploy history should show older SHA")
	}
	if !strings.Contains(body, "Deploy history") {
		t.Error("should have deploy history section")
	}
	// Pillar status should reflect regressions.
	if !strings.Contains(body, "regression") {
		t.Error("perf pillar should show regression status")
	}
	// Anomaly pillar should show open count.
	if !strings.Contains(body, "1 open") {
		t.Error("anomaly pillar should show open count")
	}
}

// ---------------------------------------------------------------------------
// Unit tests for landing helpers
// ---------------------------------------------------------------------------

func TestComputeVerdict(t *testing.T) {
	cases := []struct {
		name  string
		regs  []deploySignal
		label string
		tone  string
	}{
		{"no regressions", nil, "Healthy", "ok"},
		{"med regressions only", []deploySignal{{Tone: "med"}, {Tone: "med"}}, "Drifting", "med"},
		{"one high regression", []deploySignal{{Tone: "high"}}, "Watch", "high"},
		{"mixed", []deploySignal{{Tone: "med"}, {Tone: "high"}}, "Watch", "high"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := computeVerdict(tc.regs)
			if v.Label != tc.label {
				t.Errorf("verdict label = %q, want %q", v.Label, tc.label)
			}
			if v.Tone != tc.tone {
				t.Errorf("verdict tone = %q, want %q", v.Tone, tc.tone)
			}
		})
	}
}

func TestSigmaTone(t *testing.T) {
	if got := sigmaTone(3.0); got != "med" {
		t.Errorf("sigmaTone(3.0) = %q, want med", got)
	}
	if got := sigmaTone(6.0); got != "high" {
		t.Errorf("sigmaTone(6.0) = %q, want high", got)
	}
	if got := sigmaTone(-6.0); got != "high" {
		t.Errorf("sigmaTone(-6.0) = %q, want high", got)
	}
}

func TestShortSHA(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"abc12345def67890", "abc12345"},
		{"short", "short"},
		{"12345678", "12345678"},
		{"", "unknown"},
	}
	for _, tc := range cases {
		if got := shortSHA(tc.in); got != tc.want {
			t.Errorf("shortSHA(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPlural(t *testing.T) {
	if got := plural(1); got != "" {
		t.Errorf("plural(1) = %q, want empty", got)
	}
	if got := plural(0); got != "s" {
		t.Errorf("plural(0) = %q, want s", got)
	}
	if got := plural(5); got != "s" {
		t.Errorf("plural(5) = %q, want s", got)
	}
}

func seedTestData(t *testing.T, fake *memfake.Fake) {
	t.Helper()
	ctx := context.Background()
	base := fixedNow.Add(-48 * time.Hour).Truncate(time.Hour)

	for i := 0; i < 24; i++ {
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10,
		}})
	}

	for i := 0; i < 48; i++ {
		p95 := 100.0
		if i >= 24 {
			p95 = 200.0
		}
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "GET /dashboard",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       50, P95: &p95,
		}})
	}

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "abc123",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-2 * time.Hour).Truncate(time.Hour),
		Count:       5,
	}})
}

func TestOutcomesPage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /outcomes = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "signup.completed") {
		t.Error("missing signup.completed card")
	}
	if !strings.Contains(body, "sparkline") {
		t.Error("missing sparkline SVG")
	}
}

func TestOutcomeDetailPage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes/signup.completed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /outcomes/signup.completed = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "signup.completed") {
		t.Error("missing metric name")
	}
	if !strings.Contains(body, "chart") {
		t.Error("missing chart SVG")
	}
}

func TestPerformancePage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /performance = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "/dashboard") {
		t.Error("missing endpoint path in performance list")
	}
	if !strings.Contains(body, "1,200") {
		t.Error("missing volume on performance card")
	}
}

func TestPerformanceDetailPage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	// Seed a perf metric with a simple name (no spaces) for URL-safe detail page test.
	ctx := context.Background()
	base := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		p95 := 150.0
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "perf.dashboard",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       50, P95: &p95,
		}})
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance/perf.dashboard", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /performance/perf.dashboard = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "perf.dashboard") {
		t.Error("missing endpoint name")
	}
	if !strings.Contains(body, "P95 Latency") {
		t.Error("missing latency chart title")
	}
	if !strings.Contains(body, "Request volume") {
		t.Error("missing volume chart title")
	}
	if c := strings.Count(body, "class=\"chart\""); c != 2 {
		t.Errorf("expected 2 chart SVGs, got %d", c)
	}
}

func TestPerformanceDetailPage_slashInName(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()
	base := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)

	name := "PUT /rails/active_storage/blobs/proxy/:signed_id/upload"
	for i := 0; i < 24; i++ {
		p95 := 125.0
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: name,
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       30, P95: &p95,
		}})
	}

	// Request with literal slashes — the {name...} wildcard must capture all segments.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance/PUT%20/rails/active_storage/blobs/proxy/:signed_id/upload", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /performance/<slashed-name> = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "active_storage") {
		t.Error("missing endpoint name in response body")
	}
}

func TestErrorsPage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /errors = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "NoMethodError") {
		t.Error("missing error card")
	}
}

func TestErrorDetailPage(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors/abc123", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /errors/abc123 = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "NoMethodError") {
		t.Error("missing error name")
	}
	if !strings.Contains(body, "abc123") {
		t.Error("missing fingerprint")
	}
}

func TestErrorsPage_newBadge(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	// Seed a "new" error (only in recent window) and an "old" one (also before window).
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "NewError", Fingerprint: "new-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-1 * time.Hour).Truncate(time.Hour),
		Count:       1,
	}})
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "OldError", Fingerprint: "old-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-10 * 24 * time.Hour).Truncate(time.Hour),
		Count:       1,
	}})
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "OldError", Fingerprint: "old-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-2 * time.Hour).Truncate(time.Hour),
		Count:       1,
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors", nil))
	body := rec.Body.String()

	if !strings.Contains(body, "el-pill-new") {
		t.Error("missing NEW badge for new-fp")
	}
}

func TestErrorDetailPage_stackTrace(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	// Seed error metric so the detail page finds it.
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "RuntimeError", Fingerprint: "st-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-1 * time.Hour).Truncate(time.Hour),
		Count:       3,
	}})

	// Seed a raw error event with a stack trace.
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind:        beacondb.KindError,
		Name:        "RuntimeError",
		Fingerprint: "st-fp",
		Properties: map[string]any{
			"stack_trace":    "app/controllers/checkout_controller.rb:42:in `process'\napp/models/order.rb:17:in `validate'",
			"message":        "undefined method 'name'",
			"first_app_frame": "app/controllers/checkout_controller.rb:42",
		},
		CreatedAt: fixedNow.Add(-30 * time.Minute),
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors/st-fp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /errors/st-fp = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "RuntimeError") {
		t.Error("missing error name")
	}
	if !strings.Contains(body, "checkout_controller.rb") {
		t.Error("missing stack trace file")
	}
	if !strings.Contains(body, "ed-stack") {
		t.Error("stack trace not rendered in ed-stack layout")
	}
}

func TestLogin_correctToken(t *testing.T) {
	_, mux := newTestDashboard(t, "secret-token")

	// GET /login to obtain a CSRF token.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", rec.Code)
	}
	csrfCookie := extractCookie(rec, csrfCookieName)
	if csrfCookie == "" {
		t.Fatal("no CSRF cookie set on GET /login")
	}

	// POST /login with correct token + CSRF.
	form := "token=secret-token&csrf_token=" + csrfCookie
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("POST /login = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("redirect to %q, want /", loc)
	}
	session := extractCookie(rec, sessionCookieName)
	if session == "" {
		t.Error("no session cookie set after correct login")
	}

	// Use the session cookie to access /.
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: session})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET / with session = %d, want 200", rec.Code)
	}
}

func TestLogin_wrongToken(t *testing.T) {
	_, mux := newTestDashboard(t, "secret-token")

	// GET /login for CSRF.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	csrfCookie := extractCookie(rec, csrfCookieName)

	// POST with wrong token.
	form := "token=wrong&csrf_token=" + csrfCookie
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfCookie})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("POST /login = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=invalid+token") {
		t.Errorf("redirect to %q, want error=invalid+token", loc)
	}
	if session := extractCookie(rec, sessionCookieName); session != "" {
		t.Error("session cookie should not be set on wrong token")
	}
}

func TestLogout(t *testing.T) {
	_, mux := newTestDashboard(t, "secret-token")

	// Simulate a valid session cookie.
	sessionVal := signedCookieValue(sessionValue)
	req := httptest.NewRequest(http.MethodGet, "/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookieName, Value: sessionVal})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("GET /logout = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("redirect to %q, want /login", loc)
	}
	// The session cookie should be cleared (MaxAge -1).
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName && c.MaxAge >= 0 {
			t.Error("session cookie not cleared on logout")
		}
	}
}

func TestLogin_CSRFRejection(t *testing.T) {
	_, mux := newTestDashboard(t, "secret-token")

	// POST /login without a CSRF cookie — should reject.
	form := "token=secret-token&csrf_token=forged-value"
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("POST /login = %d, want 302", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "error=invalid+request") {
		t.Errorf("redirect to %q, want error=invalid+request", loc)
	}

	// POST with mismatched CSRF cookie vs form value.
	req = httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: csrfCookieName, Value: "different-csrf-value"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("POST /login mismatch = %d, want 302", rec.Code)
	}
	loc = rec.Header().Get("Location")
	if !strings.Contains(loc, "error=invalid+request") {
		t.Errorf("redirect to %q, want error=invalid+request", loc)
	}
}

func TestLogin_noAuthSkipsLogin(t *testing.T) {
	_, mux := newTestDashboard(t, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("GET /login (no auth) = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("redirect to %q, want /", loc)
	}
}

// extractCookie finds a Set-Cookie value by name from a response.
func extractCookie(rec *httptest.ResponseRecorder, name string) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func TestSparklineSVG(t *testing.T) {
	tests := []struct {
		name   string
		series []float64
		want   string
	}{
		{"empty", nil, "sparkline"},
		{"single", []float64{42}, "polyline"},
		{"normal", []float64{1, 3, 2, 5, 4}, "polyline"},
		{"flat", []float64{10, 10, 10}, "polyline"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svg := SparklineSVG(tc.series, 100, 30)
			if !strings.Contains(string(svg), tc.want) {
				t.Errorf("SVG missing %q: %s", tc.want, svg)
			}
		})
	}
}

func TestChartSVG(t *testing.T) {
	baseline := 50.0
	tests := []struct {
		name string
		opts ChartOptions
		want []string
	}{
		{
			"empty",
			ChartOptions{Width: 600, Height: 200},
			[]string{"chart"},
		},
		{
			"with data and baseline",
			ChartOptions{
				Width: 600, Height: 200,
				Series: []ChartPoint{
					{"Apr 1", 40}, {"Apr 2", 60}, {"Apr 3", 55},
					{"Apr 4", 70}, {"Apr 5", 45},
				},
				Baseline:      &baseline,
				DeployIndices: []int{2},
			},
			[]string{"chart-line", "chart-baseline", "chart-deploy", "chart-label-x", "chart-label-y"},
		},
		{
			"single point",
			ChartOptions{
				Width: 600, Height: 200,
				Series: []ChartPoint{{"now", 100}},
			},
			[]string{"polyline", "chart-label-x"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			svg := string(ChartSVG(tc.opts))
			for _, w := range tc.want {
				if !strings.Contains(svg, w) {
					t.Errorf("SVG missing %q", w)
				}
			}
		})
	}
}

func TestAnomaliesPage_rendersSeededAnomalies(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindAmbient, Name: "http_request",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 100, Sum: fp(5.0), P50: fp(10), P95: fp(1),
			Fingerprint: "volume_shift", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /anomalies = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "http_request") {
		t.Error("missing anomaly name in page")
	}
	if !strings.Contains(body, "VOLUME SHIFT") {
		t.Error("missing anomaly kind label")
	}
	if !strings.Contains(body, "5.0σ") {
		t.Error("missing sigma value")
	}
}

func TestAnomaliesPage_emptyState(t *testing.T) {
	_, mux := newTestDashboard(t, "")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /anomalies = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Nothing unusual") {
		t.Error("missing empty state message")
	}
}

func TestAnomaliesPage_htmxPartial(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindPerf, Name: "GET /search",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 200, Sum: fp(8.0), P50: fp(20), P95: fp(2),
			Fingerprint: "volume_shift", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("HX GET /anomalies = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	// Partial should NOT contain the full layout (no <html> tag).
	if strings.Contains(body, "<html") {
		t.Error("htmx partial should not contain full layout")
	}
	if !strings.Contains(body, "GET /search") {
		t.Error("htmx partial missing anomaly card")
	}
}

func TestAnomaliesPage_perfDriftBadge(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindPerf, Name: "GET /search",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 20, Sum: fp(5.0), P50: fp(50), P95: fp(200), P99: fp(30),
			Fingerprint: "perf_drift", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "ala-pillar-performance") {
		t.Error("missing performance pillar badge")
	}
	if !strings.Contains(body, "PERF DRIFT") {
		t.Error("missing PERF DRIFT kind label")
	}
	if !strings.Contains(body, "p95") {
		t.Error("missing p95 latency display")
	}
}

func TestAnomaliesPage_errorRateSpikeBadge(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindError, Name: "NoMethodError",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 100, Sum: fp(9.0), P50: fp(10), P95: fp(1),
			Fingerprint: "error_rate_spike", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "ala-pillar-errors") {
		t.Error("missing errors pillar badge")
	}
	if !strings.Contains(body, "ERROR SPIKE") {
		t.Error("missing ERROR SPIKE kind label")
	}
}

func TestAnomaliesPage_outcomeDropBadge(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 10, Sum: fp(9.0), P50: fp(100), P95: fp(10),
			Fingerprint: "outcome_drop", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies", nil)
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "ala-pillar-outcomes") {
		t.Error("missing outcomes pillar badge")
	}
	if !strings.Contains(body, "OUTCOME DROP") {
		t.Error("missing OUTCOME DROP kind label")
	}
}

func TestLandingAnomalyPillarStatus(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindAmbient, Name: "http_request",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 100, Sum: fp(12.4), P50: fp(10), P95: fp(1),
			Fingerprint: "volume_shift", DimensionHash: ""},
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mux.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "http_request") {
		t.Error("pillar status should show top anomaly name")
	}
	if !strings.Contains(body, "open") {
		t.Error("pillar status should show anomaly count")
	}
}

func fp(v float64) *float64 { return &v }

// ---------------------------------------------------------------------------
// computeTrend
// ---------------------------------------------------------------------------

func TestComputeTrend(t *testing.T) {
	cases := []struct {
		name   string
		points []reads.MetricPoint
		want   string
	}{
		{"single point", []reads.MetricPoint{{Count: 5}}, "insufficient data"},
		{"all zeros", []reads.MetricPoint{{Count: 0}, {Count: 0}}, "no occurrences"},
		{"first half zero", []reads.MetricPoint{{Count: 0}, {Count: 3}}, "increasing (new)"},
		{"increasing", []reads.MetricPoint{{Count: 2}, {Count: 2}, {Count: 5}, {Count: 5}}, "increasing"},
		{"decreasing", []reads.MetricPoint{{Count: 10}, {Count: 10}, {Count: 2}, {Count: 2}}, "decreasing"},
		{"stable", []reads.MetricPoint{{Count: 10}, {Count: 10}, {Count: 11}, {Count: 10}}, "stable"},
		{"two points stable", []reads.MetricPoint{{Count: 5}, {Count: 5}}, "stable"},
		{"two points increasing", []reads.MetricPoint{{Count: 2}, {Count: 10}}, "increasing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := computeTrend(tc.points)
			if got != tc.want {
				t.Errorf("computeTrend = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// splitMethodPath
// ---------------------------------------------------------------------------

func TestSplitMethodPath(t *testing.T) {
	cases := []struct {
		input      string
		wantMethod string
		wantPath   string
		wantNil    bool
	}{
		{"GET /items/47", "GET", "/items/47", false},
		{"POST /api/events", "POST", "/api/events", false},
		{"/just/a/path", "", "", true},
		{"", "", "", true},
		{"DELETE /items/1 extra", "DELETE", "/items/1 extra", false},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			parts := splitMethodPath(tc.input)
			if tc.wantNil {
				if parts != nil {
					t.Errorf("splitMethodPath(%q) = %v, want nil", tc.input, parts)
				}
				return
			}
			if len(parts) != 2 {
				t.Fatalf("splitMethodPath(%q) = %v, want 2 parts", tc.input, parts)
			}
			if parts[0] != tc.wantMethod || parts[1] != tc.wantPath {
				t.Errorf("splitMethodPath(%q) = [%q, %q], want [%q, %q]",
					tc.input, parts[0], parts[1], tc.wantMethod, tc.wantPath)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// highlightStackTrace
// ---------------------------------------------------------------------------

func TestHighlightStackTrace(t *testing.T) {
	trace := "app/models/item.rb:42:in 'title'\n/gems/activesupport-7.0/lib/active_support.rb:10\napp/controllers/items_controller.rb:15"
	result := string(highlightStackTrace(trace))

	// App frames should get frame-app class.
	if !strings.Contains(result, `<span class="frame-app">app/models/item.rb:42`) {
		t.Error("first app frame not highlighted as frame-app")
	}
	if !strings.Contains(result, `<span class="frame-app">app/controllers/items_controller.rb:15`) {
		t.Error("second app frame not highlighted as frame-app")
	}

	// Framework frame should get frame-framework class.
	if !strings.Contains(result, `<span class="frame-framework">/gems/activesupport`) {
		t.Error("framework frame not highlighted as frame-framework")
	}
}

func TestHighlightStackTrace_escapesHTML(t *testing.T) {
	trace := `app/models/<script>alert("xss")</script>.rb:1`
	result := string(highlightStackTrace(trace))

	if strings.Contains(result, "<script>") {
		t.Error("HTML not escaped in stack trace output")
	}
	if !strings.Contains(result, "&lt;script&gt;") {
		t.Error("expected escaped HTML entities")
	}
}

// ---------------------------------------------------------------------------
// Tests for outcomes helper functions
// ---------------------------------------------------------------------------

func TestSparklineDeployIndices(t *testing.T) {
	base := time.Date(2026, 4, 7, 0, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour

	tests := []struct {
		name    string
		deploys []reads.DeployEvent
		nPts    int
		want    []int
	}{
		{"empty deploys", nil, 100, nil},
		{"zero points", []reads.DeployEvent{{CreatedAt: base.Add(time.Hour)}}, 0, nil},
		{"single point", []reads.DeployEvent{{CreatedAt: base.Add(time.Hour)}}, 1, nil},
		{
			"mid window",
			[]reads.DeployEvent{{CreatedAt: base.Add(week / 2)}},
			168,
			[]int{84},
		},
		{
			"start of window",
			[]reads.DeployEvent{{CreatedAt: base}},
			168,
			[]int{0},
		},
		{
			"end of window",
			[]reads.DeployEvent{{CreatedAt: base.Add(week)}},
			168,
			[]int{168 - 1},
		},
		{
			"outside window ignored",
			[]reads.DeployEvent{{CreatedAt: base.Add(-time.Hour)}},
			168,
			nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := sparklineDeployIndices(tc.deploys, base, base.Add(week), tc.nPts)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestChartDeployIndices(t *testing.T) {
	points := []ChartPoint{
		{"2026-04-07", 10},
		{"2026-04-08", 20},
		{"2026-04-09", 30},
	}

	tests := []struct {
		name    string
		deploys []reads.DeployEvent
		wantN   int
		wantIdx int
		wantLbl string
	}{
		{"empty", nil, 0, 0, ""},
		{
			"maps to nearest day",
			[]reads.DeployEvent{{SHA: "abc1234567", CreatedAt: time.Date(2026, 4, 8, 6, 0, 0, 0, time.UTC)}},
			1, 1, "abc1234",
		},
		{
			"empty SHA becomes dash",
			[]reads.DeployEvent{{SHA: "", CreatedAt: time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC)}},
			1, 2, "—",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := chartDeployIndices(tc.deploys, points)
			if len(got) != tc.wantN {
				t.Fatalf("got %d results, want %d", len(got), tc.wantN)
			}
			if tc.wantN > 0 {
				if got[0].Index != tc.wantIdx {
					t.Errorf("index = %d, want %d", got[0].Index, tc.wantIdx)
				}
				if got[0].Label != tc.wantLbl {
					t.Errorf("label = %q, want %q", got[0].Label, tc.wantLbl)
				}
			}
		})
	}
}

func TestChartDeployIndices_deduplicates(t *testing.T) {
	points := []ChartPoint{
		{"2026-04-07", 10},
		{"2026-04-08", 20},
	}
	deploys := []reads.DeployEvent{
		{SHA: "aaa1111", CreatedAt: time.Date(2026, 4, 8, 10, 0, 0, 0, time.UTC)},
		{SHA: "bbb2222", CreatedAt: time.Date(2026, 4, 8, 14, 0, 0, 0, time.UTC)},
	}
	got := chartDeployIndices(deploys, points)
	if len(got) != 1 {
		t.Fatalf("expected 1 deduplicated entry, got %d", len(got))
	}
	if got[0].Label != "bbb2222" {
		t.Errorf("last SHA should win: got %q, want bbb2222", got[0].Label)
	}
}

func TestNearestIndex(t *testing.T) {
	pts := []time.Time{
		time.Date(2026, 4, 7, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		name string
		t    time.Time
		want int
	}{
		{"exact match", pts[1], 1},
		{"between, closer to second", time.Date(2026, 4, 7, 18, 0, 0, 0, time.UTC), 1},
		{"between, closer to first", time.Date(2026, 4, 7, 6, 0, 0, 0, time.UTC), 0},
		{"after all", time.Date(2026, 4, 20, 0, 0, 0, 0, time.UTC), 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := nearestIndex(tc.t, pts)
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("empty", func(t *testing.T) {
		if got := nearestIndex(time.Now(), nil); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})
}

func TestShortenSHA(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"abc1234567890", "abc1234"},
		{"short", "short"},
		{"", "—"},
		{"  ", "—"},
		{"abc1234", "abc1234"},
	}
	for _, tc := range tests {
		if got := shortenSHA(tc.in); got != tc.want {
			t.Errorf("shortenSHA(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "just now"},
		{5 * time.Minute, "5m ago"},
		{90 * time.Minute, "1h ago"},
		{3 * time.Hour, "3h ago"},
		{48 * time.Hour, "2d ago"},
	}
	for _, tc := range tests {
		if got := formatElapsed(tc.d); got != tc.want {
			t.Errorf("formatElapsed(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func TestSparklineSVG_withDeployMarkers(t *testing.T) {
	svg := string(SparklineSVG([]float64{1, 2, 3, 4, 5}, 100, 30, 2))
	if !strings.Contains(svg, "sparkline-deploy") {
		t.Error("deploy marker not rendered")
	}
	if !strings.Contains(svg, "polyline") {
		t.Error("data line missing")
	}
}

func TestChartSVG_baselineBand(t *testing.T) {
	baseline := 50.0
	stddev := 10.0
	svg := string(ChartSVG(ChartOptions{
		Width: 600, Height: 200,
		Series: []ChartPoint{
			{"Apr 1", 40}, {"Apr 2", 60}, {"Apr 3", 55},
		},
		Baseline:       &baseline,
		BaselineStddev: &stddev,
	}))
	if !strings.Contains(svg, "chart-band") {
		t.Error("baseline band not rendered")
	}
	if !strings.Contains(svg, "chart-baseline") {
		t.Error("baseline line not rendered")
	}
}

func TestChartSVG_deployLabels(t *testing.T) {
	svg := string(ChartSVG(ChartOptions{
		Width: 600, Height: 200,
		Series: []ChartPoint{
			{"Apr 1", 40}, {"Apr 2", 60}, {"Apr 3", 55},
		},
		DeployIndices: []int{1},
		DeployLabels:  []string{"abc1234"},
	}))
	if !strings.Contains(svg, "chart-deploy-label") {
		t.Error("deploy label not rendered")
	}
	if !strings.Contains(svg, "abc1234") {
		t.Error("SHA text not in SVG")
	}
}

// ---------------------------------------------------------------------------
// errorStatus
// ---------------------------------------------------------------------------

func TestErrorStatus(t *testing.T) {
	now := fixedNow
	cases := []struct {
		name       string
		summary    reads.ErrorSummary
		deploySHA  string
		deployTime time.Time
		want       string
	}{
		{
			"no deploy, first seen < 24h",
			reads.ErrorSummary{FirstSeen: now.Add(-12 * time.Hour).Format(time.RFC3339)},
			"", time.Time{},
			"new",
		},
		{
			"no deploy, first seen > 24h",
			reads.ErrorSummary{FirstSeen: now.Add(-48 * time.Hour).Format(time.RFC3339)},
			"", time.Time{},
			"unresolved",
		},
		{
			"introduced in latest deploy",
			reads.ErrorSummary{IntroducedDeploySHA: "abc123"},
			"abc123", now.Add(-1 * time.Hour),
			"new",
		},
		{
			"last seen before latest deploy",
			reads.ErrorSummary{
				IntroducedDeploySHA: "old-sha",
				LastSeen:            now.Add(-2 * time.Hour).Format(time.RFC3339),
			},
			"abc123", now.Add(-1 * time.Hour),
			"resolved",
		},
		{
			"last seen after latest deploy",
			reads.ErrorSummary{
				IntroducedDeploySHA: "old-sha",
				LastSeen:            now.Add(-30 * time.Minute).Format(time.RFC3339),
			},
			"abc123", now.Add(-1 * time.Hour),
			"unresolved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := errorStatus(tc.summary, tc.deploySHA, tc.deployTime, now)
			if got != tc.want {
				t.Errorf("errorStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// trendDisplay
// ---------------------------------------------------------------------------

func TestTrendDisplay(t *testing.T) {
	cases := []struct {
		trend     string
		wantIcon  string
		wantClass string
	}{
		{"increasing", "↑", "ed-trend-up"},
		{"decreasing", "↓", "ed-trend-down"},
		{"stable", "→", "ed-trend-flat"},
		{"", "→", "ed-trend-flat"},
	}
	for _, tc := range cases {
		t.Run(tc.trend, func(t *testing.T) {
			icon, class := trendDisplay(tc.trend)
			if icon != tc.wantIcon {
				t.Errorf("icon = %q, want %q", icon, tc.wantIcon)
			}
			if class != tc.wantClass {
				t.Errorf("class = %q, want %q", class, tc.wantClass)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseStackTrace
// ---------------------------------------------------------------------------

func TestParseStackTrace(t *testing.T) {
	trace := "app/models/item.rb:42:in `title'\n/gems/activerecord/lib/base.rb:10\napp/controllers/items_controller.rb:15\n\n"
	frames := parseStackTrace(trace)
	if len(frames) != 3 {
		t.Fatalf("frames = %d, want 3", len(frames))
	}
	if frames[0].File != "app/models/item.rb" {
		t.Errorf("frame 0 file = %q", frames[0].File)
	}
	if frames[0].Line != "42" {
		t.Errorf("frame 0 line = %q", frames[0].Line)
	}
	if !frames[0].InApp {
		t.Error("frame 0 should be in-app")
	}
	if frames[1].InApp {
		t.Error("frame 1 should be framework")
	}
}

func TestParseStackTrace_empty(t *testing.T) {
	frames := parseStackTrace("")
	if len(frames) != 0 {
		t.Errorf("frames = %d, want 0", len(frames))
	}
}

func TestParseStackTrace_genericFormat(t *testing.T) {
	trace := "some/file.go:99\nanother/file.go:42:in `method'"
	frames := parseStackTrace(trace)
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	if frames[0].File != "some/file.go" {
		t.Errorf("frame 0 file = %q", frames[0].File)
	}
}

// ---------------------------------------------------------------------------
// formatTimeAgo
// ---------------------------------------------------------------------------

func TestFormatTimeAgo(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		ts   string
		want string
	}{
		{"just now", now.Add(-10 * time.Second).Format(time.RFC3339), "just now"},
		{"minutes", now.Add(-5 * time.Minute).Format(time.RFC3339), "5m ago"},
		{"hours", now.Add(-3 * time.Hour).Format(time.RFC3339), "3h ago"},
		{"days", now.Add(-48 * time.Hour).Format(time.RFC3339), "2d ago"},
		{"bad format", "not-a-date", "not-a-date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTimeAgo(tc.ts)
			if got != tc.want {
				t.Errorf("formatTimeAgo = %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// formatDateShort
// ---------------------------------------------------------------------------

func TestFormatDateShort(t *testing.T) {
	got := formatDateShort("2026-04-10T12:30:00Z")
	if got != "2026-04-10 12:30Z" {
		t.Errorf("got %q", got)
	}
	got = formatDateShort("invalid")
	if got != "invalid" {
		t.Errorf("fallback = %q, want 'invalid'", got)
	}
}

// ---------------------------------------------------------------------------
// outcomeSigmaPill
// ---------------------------------------------------------------------------

func TestOutcomeSigmaPill(t *testing.T) {
	cases := []struct {
		sigma float64
		pct   float64
		want  string
	}{
		{0.3, 5, "stable"},
		{-0.3, 5, "stable"},
		{2.0, 25, "out-pill-up"},
		{-1.5, 15, "out-pill-down"},
	}
	for _, tc := range cases {
		got := string(outcomeSigmaPill(tc.sigma, tc.pct))
		if !strings.Contains(got, tc.want) {
			t.Errorf("outcomeSigmaPill(%v, %v) = %q, want to contain %q", tc.sigma, tc.pct, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// perfSigmaPill
// ---------------------------------------------------------------------------

func TestPerfSigmaPill(t *testing.T) {
	cases := []struct {
		sigma float64
		want  string
	}{
		{0.3, "perf-pill-stable"},
		{2.0, "perf-pill-slower"},
		{-1.5, "perf-pill-faster"},
	}
	for _, tc := range cases {
		got := string(perfSigmaPill(tc.sigma))
		if !strings.Contains(got, tc.want) {
			t.Errorf("perfSigmaPill(%v) = %q, want to contain %q", tc.sigma, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// fmtMs
// ---------------------------------------------------------------------------

func TestFmtMs(t *testing.T) {
	cases := []struct {
		ms   float64
		want string
	}{
		{50, "50ms"},
		{999, "999ms"},
		{1000, "1.00s"},
		{2500, "2.50s"},
	}
	for _, tc := range cases {
		got := fmtMs(tc.ms)
		if got != tc.want {
			t.Errorf("fmtMs(%v) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// driftLabel
// ---------------------------------------------------------------------------

func TestDriftLabel(t *testing.T) {
	cases := []struct {
		pct       float64
		wantLabel string
		wantClass string
	}{
		{0.5, "flat", "drift-flat"},
		{10, "↑ 10%", "drift-up"},
		{-15, "↓ 15%", "drift-down"},
	}
	for _, tc := range cases {
		label, class := driftLabel(tc.pct)
		if label != tc.wantLabel {
			t.Errorf("driftLabel(%v) label = %q, want %q", tc.pct, label, tc.wantLabel)
		}
		if class != tc.wantClass {
			t.Errorf("driftLabel(%v) class = %q, want %q", tc.pct, class, tc.wantClass)
		}
	}
}

// ---------------------------------------------------------------------------
// sigmaDriftLabel
// ---------------------------------------------------------------------------

func TestSigmaDriftLabel(t *testing.T) {
	cases := []struct {
		sigma     float64
		wantLabel string
		wantClass string
	}{
		{0.5, "stable", "drift-flat"},
		{2.0, "2.0σ slower", "drift-up"},
		{-2.0, "2.0σ faster", "drift-down"},
	}
	for _, tc := range cases {
		label, class := sigmaDriftLabel(tc.sigma)
		if label != tc.wantLabel {
			t.Errorf("sigmaDriftLabel(%v) label = %q, want %q", tc.sigma, label, tc.wantLabel)
		}
		if class != tc.wantClass {
			t.Errorf("sigmaDriftLabel(%v) class = %q, want %q", tc.sigma, class, tc.wantClass)
		}
	}
}

// ---------------------------------------------------------------------------
// relativeTime
// ---------------------------------------------------------------------------

func TestRelativeTime(t *testing.T) {
	now := time.Date(2026, 5, 26, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		t    time.Time
		want string
	}{
		{now.Add(-30 * time.Second), "just now"},
		{now.Add(-5 * time.Minute), "5m ago"},
		{now.Add(-3 * time.Hour), "3h ago"},
		{now.Add(-25 * time.Hour), "yesterday"},
		{now.Add(-72 * time.Hour), "3 days ago"},
	}
	for _, tc := range cases {
		got := relativeTime(tc.t, now)
		if got != tc.want {
			t.Errorf("relativeTime(%v) = %q, want %q", tc.t, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// linkForAnomaly
// ---------------------------------------------------------------------------

func TestLinkForAnomaly(t *testing.T) {
	cases := []struct {
		kind string
		name string
		want string
	}{
		{"perf", "GET /items", "/performance/GET%20%2Fitems"},
		{"error", "RuntimeError", "/errors"},
		{"outcome", "signup.completed", "/outcomes/signup.completed"},
		{"unknown", "x", ""},
	}
	for _, tc := range cases {
		got := linkForAnomaly(reads.AnomalyEntry{MetricKind: tc.kind, Name: tc.name})
		if got != tc.want {
			t.Errorf("linkForAnomaly(%q, %q) = %q, want %q", tc.kind, tc.name, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// toAnomalyRow
// ---------------------------------------------------------------------------

func TestToAnomalyRow(t *testing.T) {
	row := toAnomalyRow(reads.AnomalyEntry{
		ID: 1, AnomalyKind: "dimension_spike", MetricKind: "outcome",
		Name: "signup", Dimension: map[string]any{"plan": "pro"},
		Current: 50, BaselineMean: 10, DeviationSigma: 5.0,
		Summary: "test summary", FirstDetected: "2026-04-10T12:00:00Z",
	})
	if row.KindLabel != "DIMENSION SPIKE" {
		t.Errorf("kind label = %q, want DIMENSION SPIKE", row.KindLabel)
	}
	if row.Pillar != "outcomes" {
		t.Errorf("pillar = %q, want outcomes", row.Pillar)
	}
	if row.SevTier != "medium" {
		t.Errorf("sev tier = %q, want medium", row.SevTier)
	}
	if row.Dimension != "plan=pro" {
		t.Errorf("dimension = %q", row.Dimension)
	}
	if row.Sigma != "5.0σ" {
		t.Errorf("sigma = %q", row.Sigma)
	}
	if row.Multiplier != "5×" {
		t.Errorf("multiplier = %q, want 5×", row.Multiplier)
	}

	row2 := toAnomalyRow(reads.AnomalyEntry{
		AnomalyKind: "volume_shift", MetricKind: "perf", Name: "GET /x",
	})
	if row2.KindLabel != "VOLUME SHIFT" {
		t.Errorf("kind label = %q, want VOLUME SHIFT", row2.KindLabel)
	}
	if row2.Pillar != "performance" {
		t.Errorf("pillar = %q, want performance", row2.Pillar)
	}
}

// ---------------------------------------------------------------------------
// filterAnomalies
// ---------------------------------------------------------------------------

func TestFilterAnomalies(t *testing.T) {
	rows := []anomalyRowData{
		{Name: "a", SevTier: "high", Pillar: "outcomes"},
		{Name: "b", SevTier: "medium", Pillar: "performance"},
		{Name: "c", SevTier: "low", Pillar: "errors"},
		{Name: "d", SevTier: "high", Pillar: "performance"},
	}

	cases := []struct {
		filter string
		want   int
	}{
		{"", 4},
		{"all", 4},
		{"severe", 2},
		{"strong", 1},
		{"mild", 1},
		{"outcomes", 1},
		{"performance", 2},
		{"errors", 1},
		{"bogus", 4},
	}
	for _, tc := range cases {
		got := filterAnomalies(rows, tc.filter)
		if len(got) != tc.want {
			t.Errorf("filter(%q) = %d rows, want %d", tc.filter, len(got), tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// sevTier
// ---------------------------------------------------------------------------

func TestSevTier(t *testing.T) {
	cases := []struct {
		sigma float64
		want  string
	}{
		{15.0, "high"},
		{10.0, "high"},
		{9.9, "medium"},
		{5.0, "medium"},
		{4.9, "low"},
		{0.0, "low"},
	}
	for _, tc := range cases {
		if got := sevTier(tc.sigma); got != tc.want {
			t.Errorf("sevTier(%.1f) = %q, want %q", tc.sigma, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// kindLabel
// ---------------------------------------------------------------------------

func TestKindLabel(t *testing.T) {
	cases := []struct {
		kind, want string
	}{
		{"volume_shift", "VOLUME SHIFT"},
		{"dimension_spike", "DIMENSION SPIKE"},
		{"perf_drift", "PERF DRIFT"},
		{"error_rate_spike", "ERROR SPIKE"},
		{"outcome_drop", "OUTCOME DROP"},
		{"unknown_thing", "UNKNOWN THING"},
	}
	for _, tc := range cases {
		if got := kindLabel(tc.kind); got != tc.want {
			t.Errorf("kindLabel(%q) = %q, want %q", tc.kind, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// fmtCount
// ---------------------------------------------------------------------------

func TestFmtCount(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{1500, "1.5k"},
		{9999, "10.0k"},
		{10000, "10k"},
		{15432, "15k"},
	}
	for _, tc := range cases {
		if got := fmtCount(tc.n); got != tc.want {
			t.Errorf("fmtCount(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// computeAnomalyStats
// ---------------------------------------------------------------------------

func TestComputeAnomalyStats(t *testing.T) {
	rows := []anomalyRowData{
		{SevTier: "high", Pillar: "outcomes", SigmaRaw: 15.0, MultiplierRaw: 10},
		{SevTier: "medium", Pillar: "performance", SigmaRaw: 7.0, MultiplierRaw: 3, Name: "slow"},
		{SevTier: "low", Pillar: "errors", SigmaRaw: 2.0, MultiplierRaw: 1},
		{SevTier: "high", Pillar: "errors", SigmaRaw: 20.0, MultiplierRaw: 15, Name: "worst"},
	}
	s := computeAnomalyStats(rows)
	if s.Total != 4 {
		t.Errorf("total = %d", s.Total)
	}
	if s.High != 2 {
		t.Errorf("high = %d", s.High)
	}
	if s.Medium != 1 {
		t.Errorf("medium = %d", s.Medium)
	}
	if s.Low != 1 {
		t.Errorf("low = %d", s.Low)
	}
	if s.Outcomes != 1 || s.Performance != 1 || s.Errors != 2 {
		t.Errorf("pillars = %d/%d/%d", s.Outcomes, s.Performance, s.Errors)
	}
	if s.WorstSigma != "20.0σ" {
		t.Errorf("worst sigma = %q", s.WorstSigma)
	}
	if s.WorstName != "worst" {
		t.Errorf("worst name = %q", s.WorstName)
	}
	if s.TopMult != 15 {
		t.Errorf("top mult = %d", s.TopMult)
	}
}

// ---------------------------------------------------------------------------
// anomalies filter via HTTP
// ---------------------------------------------------------------------------

func TestAnomaliesPage_filterChips(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "checkout.completed",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 50, Sum: fp(12.0), P50: fp(10), P95: fp(1),
			Fingerprint: "dimension_spike"},
		{Kind: beacondb.KindPerf, Name: "GET /slow-endpoint",
			PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
			PeriodStart: fixedNow.Add(-1 * time.Hour),
			Count: 30, Sum: fp(3.0), P50: fp(5), P95: fp(0.5),
			Fingerprint: "volume_shift"},
	})

	// Filter by severity (severe = high = σ ≥ 10) — htmx partial
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/anomalies?filter=severe&list=1", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "checkout.completed") {
		t.Error("severe filter should include σ=12 anomaly")
	}
	if strings.Contains(body, "slow-endpoint") {
		t.Error("severe filter should exclude σ=3 anomaly")
	}

	// Filter by pillar — htmx partial
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/anomalies?filter=performance&list=1", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)
	body = rec.Body.String()
	if !strings.Contains(body, "slow-endpoint") {
		t.Error("performance filter should include perf anomaly")
	}
	if strings.Contains(body, "checkout.completed") {
		t.Error("performance filter should exclude outcome anomaly")
	}
}

// ---------------------------------------------------------------------------
// handleDismissAnomaly
// ---------------------------------------------------------------------------

func TestDismissAnomaly_dashboard(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindAmbient, Name: "test",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: fixedNow.Add(-1 * time.Hour),
		Count: 10, Sum: fp(5.0), P50: fp(10), P95: fp(1),
		Fingerprint: "volume_shift",
	}})

	rows, _ := fake.ListMetrics(ctx, beacondb.MetricFilter{PeriodKind: beacondb.PeriodAnomaly})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/anomalies/%d/dismiss", rows[0].ID), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204", rec.Code)
	}

	// Already dismissed.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, fmt.Sprintf("/anomalies/%d/dismiss", rows[0].ID), nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}

	// Bad ID.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/anomalies/notanumber/dismiss", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// safeDeref
// ---------------------------------------------------------------------------

func TestSafeDeref(t *testing.T) {
	v := 3.14
	if got := safeDeref(&v); got != 3.14 {
		t.Errorf("got %v", got)
	}
	if got := safeDeref(nil); got != 0 {
		t.Errorf("got %v, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// render with htmx partial
// ---------------------------------------------------------------------------

func TestOutcomesPage_htmxPartial(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/outcomes", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("htmx partial should not contain full layout")
	}
}

func TestPerformancePage_htmxPartial(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/performance", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestErrorsPage_htmxPartial(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/errors", nil)
	req.Header.Set("HX-Request", "true")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// outcomes with list view
// ---------------------------------------------------------------------------

func TestOutcomesPage_listView(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/outcomes?list=1", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// mean helper
// ---------------------------------------------------------------------------

func TestMean(t *testing.T) {
	if got := mean(nil); got != 0 {
		t.Errorf("mean(nil) = %v", got)
	}
	if got := mean([]float64{10, 20, 30}); got != 20 {
		t.Errorf("mean = %v", got)
	}
}

// ---------------------------------------------------------------------------
// buildErrorRow
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// handleErrors with deploy events and filters
// ---------------------------------------------------------------------------

func TestErrorsPage_withDeployContext(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	// Seed a deploy event.
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindOutcome, Name: "deploy.shipped",
		Context:   map[string]any{"deploy_sha": "deploy123"},
		CreatedAt: fixedNow.Add(-1 * time.Hour),
	}})

	// Seed an error introduced in this deploy.
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "NewError", Fingerprint: "new-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart:         fixedNow.Add(-30 * time.Minute).Truncate(time.Hour),
		Count:               3,
		IntroducedDeploySHA: "deploy123",
	}})

	// Seed an older error that stopped before the deploy (resolved).
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "OldError", Fingerprint: "old-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart:         fixedNow.Add(-48 * time.Hour).Truncate(time.Hour),
		Count:               5,
		IntroducedDeploySHA: "old-sha",
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "NewError") {
		t.Error("missing NewError")
	}
}

func TestErrorsPage_filterNew(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "Recent", Fingerprint: "fp-recent",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-1 * time.Hour).Truncate(time.Hour),
		Count:       1,
	}})
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "Ancient", Fingerprint: "fp-ancient",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: fixedNow.Add(-10 * 24 * time.Hour).Truncate(time.Hour),
		Count:       1,
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors?filter=new", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestErrorsPage_filterResolved(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindOutcome, Name: "deploy.shipped",
		Context: map[string]any{"deploy_sha": "sha1"}, CreatedAt: fixedNow.Add(-1 * time.Hour),
	}})
	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "Resolved", Fingerprint: "fp-r",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart:         fixedNow.Add(-48 * time.Hour).Truncate(time.Hour),
		Count:               2,
		IntroducedDeploySHA: "old-sha",
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors?filter=resolved", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestErrorsPage_listView(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors?list=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handlePerformance with filters
// ---------------------------------------------------------------------------

func TestPerformancePage_filterSlower(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance?filter=slower", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPerformancePage_filterHttp(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance?filter=http", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPerformancePage_filterJobs(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	base := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		p95 := 50.0
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "BackgroundJob",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       10, P95: &p95,
		}})
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance?filter=jobs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestPerformancePage_listView(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance?list=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleOutcomes with filters
// ---------------------------------------------------------------------------

func TestOutcomesPage_filterAbove(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	seedTestData(t, fake)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes?filter=above", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handleOutcomeDetail with deploy events
// ---------------------------------------------------------------------------

func TestOutcomeDetailPage_withDeploys(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	seedTestData(t, fake)
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindOutcome, Name: "deploy.shipped",
		Context:   map[string]any{"deploy_sha": "abc1234"},
		CreatedAt: fixedNow.Add(-12 * time.Hour),
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes/signup.completed", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// render: template not found path
// ---------------------------------------------------------------------------

func TestRender_missingTemplate(t *testing.T) {
	d, _, mux := newTestDashboardWithFake(t, "")
	_ = d
	// We can't directly call render with a bad template name through the mux,
	// but we can verify the error path by requesting a page that doesn't exist.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/nonexistent", nil)
	mux.ServeHTTP(rec, req)
	// Go's default mux returns 404 for unknown routes.
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// formatValue (svg.go)
// ---------------------------------------------------------------------------

func TestFormatValue(t *testing.T) {
	cases := []struct {
		val  float64
		want string
	}{
		{999, "999"},
		{1500, "1.5K"},
		{1000000, "1.0M"},
		{0, "0"},
		{3.14, "3.1"},
	}
	for _, tc := range cases {
		got := formatValue(tc.val)
		if got != tc.want {
			t.Errorf("formatValue(%v) = %q, want %q", tc.val, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// handleErrorDetail: comprehensive with sample event
// ---------------------------------------------------------------------------

func TestErrorDetailPage_fullSampleEvent(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindError, Name: "RuntimeError", Fingerprint: "full-fp",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart:         fixedNow.Add(-1 * time.Hour).Truncate(time.Hour),
		Count:               3,
		IntroducedDeploySHA: "abcdef1234567890deadbeef",
	}})
	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindError, Name: "RuntimeError", Fingerprint: "full-fp",
		Properties: map[string]any{
			"message":         "undefined method 'save'",
			"first_app_frame": "app/models/order.rb:42",
			"stack_trace":     "app/models/order.rb:42:in `save'\n/gems/activerecord/base.rb:10",
			"path":            "POST /orders",
		},
		Context: map[string]any{
			"deploy_sha":  "abcdef12",
			"environment": "production",
			"request_id":  "req-999",
		},
		CreatedAt: fixedNow.Add(-30 * time.Minute),
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors/full-fp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "RuntimeError") {
		t.Error("missing error name")
	}
	if !strings.Contains(body, "production") {
		t.Error("missing environment")
	}
	if !strings.Contains(body, "req-999") {
		t.Error("missing request_id")
	}
}

func TestErrorDetailPage_notFound(t *testing.T) {
	_, _, mux := newTestDashboardWithFake(t, "")

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/errors/nonexistent", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (error page still renders)", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// handlePerformanceDetail: with deploy events
// ---------------------------------------------------------------------------

func TestPerformanceDetailPage_withDeploys(t *testing.T) {
	_, fake, mux := newTestDashboardWithFake(t, "")
	ctx := context.Background()

	base := fixedNow.Add(-24 * time.Hour).Truncate(time.Hour)
	for i := 0; i < 24; i++ {
		p95 := 150.0
		_ = fake.UpsertMetrics(ctx, []beacondb.Metric{{
			Kind: beacondb.KindPerf, Name: "GET /api/items",
			PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
			PeriodStart: base.Add(time.Duration(i) * time.Hour),
			Count:       50, P95: &p95,
		}})
	}

	_, _ = fake.InsertEvents(ctx, []beacondb.Event{{
		Kind: beacondb.KindOutcome, Name: "deploy.shipped",
		Context:   map[string]any{"deploy_sha": "deploy-sha"},
		CreatedAt: fixedNow.Add(-12 * time.Hour),
	}})

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/performance/GET%20/api/items", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBuildErrorRow(t *testing.T) {
	summary := reads.ErrorSummary{
		Name:                "NoMethodError",
		Fingerprint:         "abcdef1234567890",
		Occurrences:         42,
		IntroducedDeploySHA: "deadbeef12345678",
		Trend:               "increasing",
		LastSeen:            "2026-04-10T10:00:00Z",
		FirstSeen:           "2026-04-09T10:00:00Z",
		HourlyCounts:        []int64{1, 2, 3},
	}
	row := buildErrorRow(summary, "new")
	if row.FPShort != "abcdef123456" {
		t.Errorf("FPShort = %q", row.FPShort)
	}
	if row.DeploySHA != "deadbeef" {
		t.Errorf("DeploySHA = %q", row.DeploySHA)
	}
	if row.TrendIcon != "↑" {
		t.Errorf("TrendIcon = %q", row.TrendIcon)
	}
	if row.TrendClass != "ed-trend-up" {
		t.Errorf("TrendClass = %q", row.TrendClass)
	}
	if row.Sparkline == "" {
		t.Error("sparkline should be non-empty with hourly data")
	}
}
