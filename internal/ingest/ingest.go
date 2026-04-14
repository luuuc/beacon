// Package ingest is Beacon's HTTP write path: POST /api/events.
//
// Responsibilities:
//
//   - Bearer auth (when configured)
//   - Per-IP rate limit (token bucket, 100 rps default)
//   - Body size limit (1 MB default) via http.MaxBytesReader
//   - Batch size limit (1000 events default)
//   - Envelope validation + per-kind field lifting (envelope.go)
//   - Clock-skew rewrite (>5min future → now, >24h past → warn)
//   - 10-minute idempotency ring via Idempotency-Key
//   - Delegates the final write to a beacondb.Adapter
//
// Error response matrix: 202/400/401/413/429 per .doc/definition/06-http-api.md.
// A storage-side failure maps to 503 (not in the spec matrix, but the honest
// answer for "we validated you fine, we just can't talk to the DB").
package ingest

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/config"
)

type Config struct {
	AuthToken         string
	MaxEventsPerBatch int
	MaxBodyBytes      int64
	RatePerSecond     float64
	IdempotencyTTL    time.Duration
	// IdempMaxEntries bounds the in-memory idempotency map. A runaway
	// client sending unique Idempotency-Key values otherwise grows the
	// map until the next sweep. Zero falls back to the store's default.
	IdempMaxEntries int
	// TrustXFF controls whether X-Forwarded-For is read when deriving the
	// per-IP rate-limiter key. Default false: a forged header could
	// otherwise move a caller to an unbucketed slot. Enable only behind a
	// proxy that rewrites XFF and a network that prevents direct reach.
	TrustXFF bool
}

func (c Config) withDefaults() Config {
	if c.MaxEventsPerBatch == 0 {
		c.MaxEventsPerBatch = 1000
	}
	if c.MaxBodyBytes == 0 {
		c.MaxBodyBytes = 1 << 20 // 1 MB
	}
	if c.RatePerSecond == 0 {
		c.RatePerSecond = 100
	}
	if c.IdempotencyTTL == 0 {
		c.IdempotencyTTL = 10 * time.Minute
	}
	return c
}

// Handler is the POST /api/events HTTP handler. It implements http.Handler.
type Handler struct {
	cfg            Config
	adapter        beacondb.Adapter
	log            *slog.Logger
	rl             *rateLimiter
	idemp          *idempStore
	now            func() time.Time
	filter         *config.PathFilter
	filteredTotal  atomic.Int64
}

// NewHandler wires all the ingest dependencies into a single http.Handler.
// The filter is optional — pass nil to disable path filtering.
func NewHandler(cfg Config, adapter beacondb.Adapter, log *slog.Logger, filter *config.PathFilter) *Handler {
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	idemp := newIdempStore(cfg.IdempotencyTTL)
	if cfg.IdempMaxEntries > 0 {
		idemp.maxEntries = cfg.IdempMaxEntries
	}
	return &Handler{
		cfg:     cfg,
		adapter: adapter,
		log:     log,
		rl:      newRateLimiter(cfg.RatePerSecond, cfg.RatePerSecond),
		idemp:   idemp,
		now:     time.Now,
		filter:  filter,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ServeMux's method-prefixed pattern ("POST /api/events") takes care of
	// method filtering; we just handle POST.

	if h.cfg.AuthToken != "" && !checkBearer(r.Header.Get("Authorization"), h.cfg.AuthToken) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	ip := clientIP(r, h.cfg.TrustXFF)
	if allowed, retry := h.rl.allow(ip); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		writeError(w, http.StatusTooManyRequests, "rate limited")
		return
	}

	idempKey := r.Header.Get("Idempotency-Key")
	if h.idemp.wasSeen(idempKey) {
		writeJSON(w, http.StatusAccepted, map[string]any{"received": 0, "duplicate": true})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var batch batchRequest
	if err := dec.Decode(&batch); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "batch exceeds body size limit")
			return
		}
		writeBadRequest(w, "malformed JSON: "+err.Error(), 0)
		return
	}

	if len(batch.Events) == 0 {
		writeBadRequest(w, "events array is empty", 0)
		return
	}
	if len(batch.Events) > h.cfg.MaxEventsPerBatch {
		writeError(w, http.StatusRequestEntityTooLarge, "batch exceeds event count limit")
		return
	}

	now := h.now()
	events := make([]beacondb.Event, 0, len(batch.Events))
	for i, env := range batch.Events {
		ev, warnings, err := env.toEvent(now)
		if err != nil {
			writeBadRequest(w,
				"events["+strconv.Itoa(i)+"]: "+err.Error(),
				len(batch.Events))
			return
		}
		for _, warn := range warnings {
			h.log.Warn("ingest warning",
				"warning", warn,
				"kind", string(ev.Kind),
				"name", ev.Name,
				"ip", ip,
			)
		}
		events = append(events, ev)
	}

	// Drop perf events whose path matches an excluded pattern. The filter
	// runs after validation so malformed events are still rejected with 400.
	if h.filter != nil {
		// In-place filter: reuses backing array; events must not be read after this block.
		filtered := events[:0]
		var dropped int64
		for _, ev := range events {
			if ev.Kind == beacondb.KindPerf {
				if p := extractPath(ev.Name); h.filter.ShouldExclude(p) {
					dropped++
					continue
				}
			}
			filtered = append(filtered, ev)
		}
		if dropped > 0 {
			h.filteredTotal.Add(dropped)
		}
		events = filtered
	}

	if len(events) == 0 {
		// Entire batch was filtered out — still 202, nothing to write.
		h.idemp.record(idempKey)
		writeJSON(w, http.StatusAccepted, map[string]any{"received": 0, "filtered": true})
		return
	}

	ctx := r.Context()
	if _, err := h.adapter.InsertEvents(ctx, events); err != nil {
		h.log.Error("ingest: InsertEvents", "err", err, "count", len(events))
		writeError(w, http.StatusServiceUnavailable, "storage error")
		return
	}

	// Record the idempotency key only after a successful write. A client
	// whose first attempt 400s can retry with the same key after fixing
	// the bug, which is the kinder semantics.
	h.idemp.record(idempKey)

	writeJSON(w, http.StatusAccepted, map[string]any{"received": len(events)})
}

func clientIP(r *http.Request, trustXFF bool) string {
	if trustXFF {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i > 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeBadRequest(w http.ResponseWriter, msg string, rejected int) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":           msg,
		"events_rejected": rejected,
	})
}

// extractPath returns the path portion of a perf event name like "GET /items/123".
// If there's no space (unexpected), the full name is returned as-is.
func extractPath(name string) string {
	if i := strings.IndexByte(name, ' '); i >= 0 {
		return name[i+1:]
	}
	return name
}

// FilteredEventsTotal returns the cumulative count of events dropped by the
// path filter since this handler was created.
func (h *Handler) FilteredEventsTotal() int64 {
	return h.filteredTotal.Load()
}

// FilterPatterns returns the active exclusion patterns, or nil if no filter
// is configured.
func (h *Handler) FilterPatterns() []string {
	if h.filter == nil {
		return nil
	}
	return h.filter.Patterns()
}

// StatsHandler returns an http.Handler for GET /api/stats that exposes
// filter counters and active patterns.
func (h *Handler) StatsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.cfg.AuthToken != "" && !checkBearer(r.Header.Get("Authorization"), h.cfg.AuthToken) {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"filtered_events_total": h.FilteredEventsTotal(),
			"filter_patterns":       h.FilterPatterns(),
		})
	})
}

// Compile-time assertion that Handler satisfies http.Handler.
var _ http.Handler = (*Handler)(nil)
