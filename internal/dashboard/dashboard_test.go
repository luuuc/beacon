package dashboard

import (
	"context"
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

	// Should have three headline cards.
	if !strings.Contains(body, "Outcomes") {
		t.Error("missing Outcomes card")
	}
	if !strings.Contains(body, "signup.completed") {
		t.Error("missing signup.completed in outcomes card")
	}
	if !strings.Contains(body, "Performance") {
		t.Error("missing Performance card")
	}
	if !strings.Contains(body, "GET /dashboard") {
		t.Error("missing GET /dashboard in performance card")
	}
	if !strings.Contains(body, "Errors") {
		t.Error("missing Errors card")
	}
	if !strings.Contains(body, "NoMethodError") {
		t.Error("missing NoMethodError in errors card")
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
	if !strings.Contains(body, "GET /dashboard") {
		t.Error("missing endpoint card")
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
	if !strings.Contains(body, "chart") {
		t.Error("missing chart SVG")
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

	if !strings.Contains(body, "badge-new") {
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
	if !strings.Contains(body, "checkout_controller.rb:42") {
		t.Error("missing stack trace content")
	}
	if !strings.Contains(body, "<pre>") {
		t.Error("stack trace not in <pre> block")
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
