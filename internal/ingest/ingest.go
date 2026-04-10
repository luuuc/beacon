// Package ingest is Beacon's HTTP write path: POST /events.
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
// Error response matrix: 202/400/401/413/429 per doc/definition/06-http-api.md.
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
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

type Config struct {
	AuthToken         string
	MaxEventsPerBatch int
	MaxBodyBytes      int64
	RatePerSecond     float64
	IdempotencyTTL    time.Duration
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

// Handler is the POST /events HTTP handler. It implements http.Handler.
type Handler struct {
	cfg     Config
	adapter beacondb.Adapter
	log     *slog.Logger
	rl      *rateLimiter
	idemp   *idempStore
	now     func() time.Time
}

// NewHandler wires all the ingest dependencies into a single http.Handler.
func NewHandler(cfg Config, adapter beacondb.Adapter, log *slog.Logger) *Handler {
	cfg = cfg.withDefaults()
	if log == nil {
		log = slog.Default()
	}
	return &Handler{
		cfg:     cfg,
		adapter: adapter,
		log:     log,
		rl:      newRateLimiter(cfg.RatePerSecond, cfg.RatePerSecond),
		idemp:   newIdempStore(cfg.IdempotencyTTL),
		now:     time.Now,
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// ServeMux's method-prefixed pattern ("POST /events") takes care of
	// method filtering; we just handle POST.

	if h.cfg.AuthToken != "" && !checkBearer(r.Header.Get("Authorization"), h.cfg.AuthToken) {
		writeError(w, http.StatusUnauthorized, "invalid token")
		return
	}

	ip := clientIP(r)
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

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i > 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
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

// Compile-time assertion that Handler satisfies http.Handler.
var _ http.Handler = (*Handler)(nil)
