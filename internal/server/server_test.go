package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/config"
)

func newTestServer(t *testing.T, checks ReadyChecks, pprofOn bool) *Server {
	t.Helper()
	cfg := config.Defaults()
	cfg.Server.PprofEnabled = pprofOn
	return New(&cfg, checks, nil)
}

func TestHealthz(t *testing.T) {
	s := newTestServer(t, ReadyChecks{}, false)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz = %d, want 200", rec.Code)
	}
}

func TestReadyzAllUnwired(t *testing.T) {
	s := newTestServer(t, ReadyChecks{}, false)
	body := doReadyz(t, s)
	if body.Status != "not ready" {
		t.Errorf("status = %q", body.Status)
	}
	for _, name := range []string{"db_reachable", "migrations_applied", "rollup_recent"} {
		if body.Checks[name] != "not wired" {
			t.Errorf("%s = %q, want 'not wired'", name, body.Checks[name])
		}
	}
}

func TestReadyzAllOK(t *testing.T) {
	ok := func(context.Context) error { return nil }
	s := newTestServer(t, ReadyChecks{
		DBReachable:       ok,
		MigrationsApplied: ok,
		RollupRecent:      ok,
	}, false)

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz = %d, want 200. body=%s", rec.Code, rec.Body.String())
	}
	var body readyResult
	mustDecode(t, rec.Body, &body)
	if body.Status != "ready" {
		t.Errorf("status = %q", body.Status)
	}
}

func TestReadyzOneFailing(t *testing.T) {
	ok := func(context.Context) error { return nil }
	fail := func(context.Context) error { return errors.New("boom") }
	s := newTestServer(t, ReadyChecks{
		DBReachable:       ok,
		MigrationsApplied: fail,
		RollupRecent:      ok,
	}, false)
	body := doReadyz(t, s)
	if body.Status != "not ready" {
		t.Errorf("status = %q", body.Status)
	}
	if body.Checks["migrations_applied"] != "fail: boom" {
		t.Errorf("migrations_applied = %q", body.Checks["migrations_applied"])
	}
	if body.Checks["db_reachable"] != "ok" {
		t.Errorf("db_reachable = %q", body.Checks["db_reachable"])
	}
}

func TestPprofDisabledByDefault(t *testing.T) {
	s := newTestServer(t, ReadyChecks{}, false)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("pprof reachable when disabled: %d", rec.Code)
	}
}

func TestPprofEnabled(t *testing.T) {
	s := newTestServer(t, ReadyChecks{}, true)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/debug/pprof/", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("pprof unreachable when enabled: %d", rec.Code)
	}
}

func TestRollupTickCheck(t *testing.T) {
	var tick time.Time
	check := RollupTickCheck(func() time.Time { return tick }, 5*time.Minute)

	if err := check(context.Background()); err == nil {
		t.Error("zero tick should fail")
	}
	tick = time.Now().Add(-10 * time.Minute)
	if err := check(context.Background()); err == nil {
		t.Error("stale tick should fail")
	}
	tick = time.Now().Add(-1 * time.Minute)
	if err := check(context.Background()); err != nil {
		t.Errorf("recent tick should pass: %v", err)
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	cfg := config.Defaults()
	cfg.Server.HTTPPort = 0 // not actually used because New binds, but harmless
	// Bind on an ephemeral port instead.
	s := New(&cfg, ReadyChecks{}, nil)
	s.http.Addr = "127.0.0.1:0"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	// Give the listener a moment to come up, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func doReadyz(t *testing.T, s *Server) readyResult {
	t.Helper()
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable && rec.Code != http.StatusOK {
		t.Fatalf("readyz unexpected code %d", rec.Code)
	}
	var body readyResult
	mustDecode(t, rec.Body, &body)
	return body
}

func mustDecode(t *testing.T, r io.Reader, v any) {
	t.Helper()
	if err := json.NewDecoder(r).Decode(v); err != nil {
		t.Fatalf("decode: %v", err)
	}
}
