// Package server hosts Beacon's HTTP surface: /healthz, /readyz, and the
// optional pprof endpoints. Ingestion and read paths land in later cards and
// will register their handlers through Server.Mux.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	"github.com/luuuc/beacon/internal/config"
)

// ReadyCheck is a single /readyz condition. nil return means healthy.
type ReadyCheck func(ctx context.Context) error

// ReadyChecks holds the three conditions /readyz reports. A nil field is
// surfaced as "not wired" — card 1 ships with all three unwired; the DB
// adapter and rollup worker cards replace them.
type ReadyChecks struct {
	DBReachable       ReadyCheck
	MigrationsApplied ReadyCheck
	RollupRecent      ReadyCheck
}

type Server struct {
	cfg    *config.Config
	checks ReadyChecks
	log    *slog.Logger
	mux    *http.ServeMux
	http   *http.Server
}

func New(cfg *config.Config, checks ReadyChecks, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	s := &Server{cfg: cfg, checks: checks, log: log, mux: http.NewServeMux()}

	s.mux.HandleFunc("/api/healthz", s.handleHealthz)
	s.mux.HandleFunc("/api/readyz", s.handleReadyz)

	if cfg.Server.PprofEnabled {
		s.mux.HandleFunc("/debug/pprof/", pprof.Index)
		s.mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		s.mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		s.mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		s.mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	}

	s.http = &http.Server{
		Addr:              net.JoinHostPort(cfg.Server.Bind, strconv.Itoa(cfg.Server.HTTPPort)),
		Handler:           s.mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler exposes the underlying mux for tests and future handler registration.
func (s *Server) Handler() http.Handler { return s.mux }

// Mount registers an additional handler on the server's mux. Must be called
// before Run — http.ServeMux is not safe for concurrent mutation once the
// server has started serving. `pattern` follows Go 1.22's method-prefixed
// form (e.g. "POST /api/events").
func (s *Server) Mount(pattern string, h http.Handler) {
	s.mux.Handle(pattern, h)
}

// Addr is the "host:port" the server will listen on.
func (s *Server) Addr() string { return s.http.Addr }

// Run starts the listener and blocks until ctx is cancelled, then performs a
// graceful shutdown with a 10s deadline.
func (s *Server) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", s.http.Addr, err)
	}
	s.log.Info("http listener up",
		"addr", ln.Addr().String(),
		"pprof", s.cfg.Server.PprofEnabled,
	)

	errCh := make(chan error, 1)
	go func() {
		err := s.http.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := s.http.Shutdown(shutCtx); err != nil {
			s.log.Warn("http shutdown", "err", err)
		}
		return <-errCh
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type readyResult struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks"`
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()

	result := readyResult{Checks: map[string]string{}}
	allOK := true
	run := func(name string, check ReadyCheck) {
		if check == nil {
			result.Checks[name] = "not wired"
			allOK = false
			return
		}
		if err := check(ctx); err != nil {
			result.Checks[name] = "fail: " + err.Error()
			allOK = false
			return
		}
		result.Checks[name] = "ok"
	}
	run("db_reachable", s.checks.DBReachable)
	run("migrations_applied", s.checks.MigrationsApplied)
	run("rollup_recent", s.checks.RollupRecent)

	if allOK {
		result.Status = "ready"
		writeJSON(w, http.StatusOK, result)
		return
	}
	result.Status = "not ready"
	writeJSON(w, http.StatusServiceUnavailable, result)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// RollupTickCheck builds the /readyz rollup_recent check. It fails when the
// last tick is zero (worker never ran) or older than maxAge.
func RollupTickCheck(lastTick func() time.Time, maxAge time.Duration) ReadyCheck {
	return func(_ context.Context) error {
		t := lastTick()
		if t.IsZero() {
			return errors.New("rollup worker has never ticked")
		}
		if age := time.Since(t); age > maxAge {
			return fmt.Errorf("last tick %s ago (max %s)", age.Round(time.Second), maxAge)
		}
		return nil
	}
}
