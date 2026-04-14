// Package rollup is Beacon's background aggregation worker.
//
// Design choices (see .doc/definition/02-architecture.md and 03-data-model.md):
//
//   - No cursor table. Every tick re-derives rollups from raw events over a
//     bounded window. Missing a tick is harmless: the next one catches up.
//     This is the "crash-safe" property — a half-finished tick leaves state
//     consistent because the work is idempotent.
//   - Each tick re-aggregates the current hour and the previous hour.
//     Previous-hour coverage catches late-arriving events that cross the
//     boundary and eliminates the need for explicit "finalize the hour" state.
//   - Percentiles are computed in Go, not in SQL. PostgreSQL has
//     percentile_cont but SQLite doesn't; doing it in Go keeps the adapter
//     interface narrow.
//   - LastTick is a monotonic stamp readable from the /readyz check. If
//     aggregation fails, LastTick doesn't advance, and readyz turns 503.
package rollup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

type Config struct {
	TickInterval     time.Duration  // default 60s
	RetentionRaw     time.Duration  // default 14 days
	AmbientRetention time.Duration  // default 24h; ambient events prune earlier than standard
	PruneAt          string         // "HH:MM" in Timezone; default "03:00"
	Timezone         *time.Location // default UTC
	Anomaly          AnomalyConfig  // anomaly detector settings
	// Now overrides the worker's clock. Leave nil in production; tests
	// inject a fixed function to exercise boundary logic deterministically.
	Now func() time.Time
}

// AnomalyConfig controls the sigma-threshold anomaly detector.
type AnomalyConfig struct {
	BaselineWindow  time.Duration // default 14d; trailing window for baseline computation
	DetectionWindow time.Duration // default 24h; current window to compare against baseline
	SigmaThreshold  float64       // default 3.0; sigma deviation to trigger anomaly
	MinVolume       int64         // default 10; suppress anomalies on low-traffic metrics
}

func (c Config) withDefaults() Config {
	if c.TickInterval <= 0 {
		c.TickInterval = time.Minute
	}
	if c.RetentionRaw <= 0 {
		c.RetentionRaw = 14 * 24 * time.Hour
	}
	if c.AmbientRetention <= 0 {
		c.AmbientRetention = 24 * time.Hour
	}
	if c.Anomaly.BaselineWindow <= 0 {
		c.Anomaly.BaselineWindow = 14 * 24 * time.Hour
	}
	if c.Anomaly.DetectionWindow <= 0 {
		c.Anomaly.DetectionWindow = 24 * time.Hour
	}
	if c.Anomaly.SigmaThreshold <= 0 {
		c.Anomaly.SigmaThreshold = 3.0
	}
	if c.Anomaly.MinVolume <= 0 {
		c.Anomaly.MinVolume = 10
	}
	if c.PruneAt == "" {
		c.PruneAt = "03:00"
	}
	if c.Timezone == nil {
		c.Timezone = time.UTC
	}
	return c
}

type Worker struct {
	cfg     Config
	adapter beacondb.Adapter
	log     *slog.Logger

	// lastTick is the unix-nanosecond stamp of the most recent successful
	// tick. Zero until the first tick completes. Readable from any goroutine.
	lastTick atomic.Int64

	// Clock and persistent tick state (guarded by the caller's single-
	// goroutine Run loop; no extra locking needed).
	now              func() time.Time
	lastPruneDate    string
	lastAnomalyDate  string
	// hourlyDoneFor is the hour bucket for which hourly-boundary work
	// (previous-hour aggregation + trailing baselines) has already run.
	// Zero until the first tick, then advances once per wall-clock hour.
	hourlyDoneFor time.Time
}

func NewWorker(cfg Config, adapter beacondb.Adapter, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Worker{
		cfg:     cfg.withDefaults(),
		adapter: adapter,
		log:     log,
		now:     now,
	}
}

// LastTick returns the timestamp of the most recent successful aggregation,
// or the zero time if no tick has completed yet. Exposed for readyz.
func (w *Worker) LastTick() time.Time {
	ns := w.lastTick.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns).UTC()
}

// Run starts the worker loop. It performs an initial tick before entering
// the ticker (the "recompute current hour on boot" crash-safety step) and
// blocks until ctx is cancelled. Errors on individual ticks are logged;
// only a context cancel ends the loop.
func (w *Worker) Run(ctx context.Context) error {
	if err := w.RunOnce(ctx); err != nil {
		w.log.Error("initial rollup tick", "err", err)
	}

	t := time.NewTicker(w.cfg.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := w.RunOnce(ctx); err != nil {
				w.log.Error("rollup tick", "err", err)
			}
		}
	}
}

// RunOnce performs one aggregation pass and runs the daily prune if the
// configured time has passed. The current hour is re-aggregated every tick;
// the previous hour and the trailing baselines are only recomputed once per
// wall-clock hour (on the first tick after the boundary crosses, and on the
// very first tick after boot for crash-safety). Exposed for tests and for
// the initial boot-time recompute.
func (w *Worker) RunOnce(ctx context.Context) error {
	now := w.now().UTC()
	currentHour := now.Truncate(time.Hour)
	hourlyBoundary := !w.hourlyDoneFor.Equal(currentHour)

	// Previous-hour sweep catches late-arriving events that crossed the
	// boundary. Only needed once per hour transition (and once on boot).
	if hourlyBoundary {
		previousHour := currentHour.Add(-time.Hour)
		if err := w.aggregateHour(ctx, previousHour); err != nil {
			return fmt.Errorf("aggregate previous hour %s: %w", previousHour.Format(time.RFC3339), err)
		}
	}

	if err := w.aggregateHour(ctx, currentHour); err != nil {
		return fmt.Errorf("aggregate current hour %s: %w", currentHour.Format(time.RFC3339), err)
	}

	// Trailing baselines fold every hourly row in the 30d window; run them
	// after the current-hour metrics have been written so the most recent
	// hour is included. Gated to the hourly boundary to avoid a 30-day
	// re-scan every tick.
	if hourlyBoundary {
		if err := w.aggregateTrailingBaselines(ctx); err != nil {
			return fmt.Errorf("aggregate trailing baselines: %w", err)
		}
		w.hourlyDoneFor = currentHour
	}

	if err := w.captureDeploymentBaselines(ctx); err != nil {
		return fmt.Errorf("capture deployment baselines: %w", err)
	}

	// Daily prune runs at most once per calendar day in the configured TZ.
	// Two-tier retention: ambient events prune at AmbientRetention (default
	// 24h), all other kinds at RetentionRaw (default 14d).
	if w.shouldPrune(now) {
		// Ambient events: short retention (24h default).
		ambientCutoff := now.Add(-w.cfg.AmbientRetention)
		an, aerr := w.adapter.DeleteEventsByKindOlderThan(ctx, beacondb.KindAmbient, ambientCutoff)
		if aerr != nil {
			w.log.Error("prune ambient events",
				"event", "prune_failure",
				"kind", "ambient",
				"cutoff_utc", ambientCutoff.Format(time.RFC3339),
				"err", aerr,
			)
		} else if an > 0 {
			w.log.Info("pruned ambient events",
				"event", "prune_completed",
				"kind", "ambient",
				"deleted", an,
				"cutoff_utc", ambientCutoff.Format(time.RFC3339),
			)
		}

		// Standard events: standard retention (14d default). This also acts
		// as a backstop for ambient events if the kind-filtered prune above
		// failed — any ambient event older than 14d will be caught here.
		cutoff := now.Add(-w.cfg.RetentionRaw)
		n, err := w.adapter.DeleteEventsOlderThan(ctx, cutoff)
		if err != nil {
			// Prune failure is non-fatal — log it and keep the tick marked
			// successful so readyz stays green. The `event=prune_failure`
			// field is the grep target documented in 09-runbook.md.
			w.log.Error("prune raw events",
				"event", "prune_failure",
				"cutoff_utc", cutoff.Format(time.RFC3339),
				"err", err,
			)
		} else {
			w.log.Info("pruned raw events",
				"event", "prune_completed",
				"deleted", n,
				"cutoff_utc", cutoff.Format(time.RFC3339),
			)
		}
	}

	// Anomaly detection runs once per day, after pruning completes.
	if w.shouldDetectAnomalies(now) {
		if err := w.detectAnomalies(ctx); err != nil {
			w.log.Error("anomaly detection",
				"event", "anomaly_detection_failure",
				"err", err,
			)
		}
	}

	w.lastTick.Store(now.UnixNano())
	return nil
}

// aggregateHour re-derives every (kind, name[, fingerprint]) rollup for the
// hour bucket [hour, hour+1) from raw events. It is safe to call repeatedly.
func (w *Worker) aggregateHour(ctx context.Context, hour time.Time) error {
	nextHour := hour.Add(time.Hour)
	events, err := w.adapter.ListEvents(ctx, beacondb.EventFilter{
		Since: hour,
		Until: nextHour,
	})
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}

	type bucketKey struct {
		kind          beacondb.Kind
		name          string
		fingerprint   string
		dimensionHash string
	}

	type bucket struct {
		events     []beacondb.Event
		dimensions map[string]any
	}

	buckets := map[bucketKey]*bucket{}

	addToBucket := func(bk bucketKey, e beacondb.Event, dims map[string]any) {
		b, ok := buckets[bk]
		if !ok {
			b = &bucket{dimensions: dims}
			buckets[bk] = b
		}
		b.events = append(b.events, e)
	}

	for _, e := range events {
		bk := bucketKey{kind: e.Kind, name: e.Name}
		if e.Kind == beacondb.KindError {
			bk.fingerprint = e.Fingerprint
		}
		// Undimensioned aggregate (always produced).
		addToBucket(bk, e, nil)

		// Per-dimension slice (only when event carries dimensions).
		if len(e.Dimensions) > 0 {
			if dh, err := beacondb.DimensionHash(e.Dimensions); err != nil {
				w.log.Warn("dimension hash failed; skipping dimensioned rollup",
					"name", e.Name, "err", err)
			} else {
				dimBK := bk
				dimBK.dimensionHash = dh
				addToBucket(dimBK, e, e.Dimensions)
			}
		}
	}

	metrics := make([]beacondb.Metric, 0, len(buckets))
	for bk, b := range buckets {
		m := beacondb.Metric{
			Kind:          bk.kind,
			Name:          bk.name,
			PeriodKind:    beacondb.PeriodHour,
			PeriodWindow:  "hour",
			PeriodStart:   hour,
			Count:         int64(len(b.events)),
			Fingerprint:   bk.fingerprint,
			Dimensions:    b.dimensions,
			DimensionHash: bk.dimensionHash,
		}

		durations := make([]float64, 0, len(b.events))
		var sum float64
		for _, e := range b.events {
			if e.DurationMs != nil {
				d := float64(*e.DurationMs)
				durations = append(durations, d)
				sum += d
			}
		}
		if len(durations) > 0 {
			sort.Float64s(durations)
			sumVal := sum
			m.Sum = &sumVal
			p50 := percentile(durations, 0.50)
			p95 := percentile(durations, 0.95)
			p99 := percentile(durations, 0.99)
			m.P50 = &p50
			m.P95 = &p95
			m.P99 = &p99
		}
		metrics = append(metrics, m)
	}

	return w.adapter.UpsertMetrics(ctx, metrics)
}

// shouldPrune returns true at most once per calendar day (in the configured
// timezone) once the wall clock is past PruneAt.
func (w *Worker) shouldPrune(nowUTC time.Time) bool {
	local := nowUTC.In(w.cfg.Timezone)
	today := local.Format("2006-01-02")
	if today == w.lastPruneDate {
		return false
	}
	hh, mm, err := parseHHMM(w.cfg.PruneAt)
	if err != nil {
		// Malformed prune_at at boot time would be a config bug. Log once
		// per tick at warn level and skip pruning.
		w.log.Warn("malformed rollup.prune_at; skipping prune", "value", w.cfg.PruneAt, "err", err)
		return false
	}
	pruneAt := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, w.cfg.Timezone)
	if local.Before(pruneAt) {
		return false
	}
	w.lastPruneDate = today
	return true
}

func parseHHMM(s string) (int, int, error) {
	if len(s) < 4 || len(s) > 5 {
		return 0, 0, errors.New("expected HH:MM")
	}
	var hh, mm int
	n, err := fmt.Sscanf(s, "%d:%d", &hh, &mm)
	if err != nil || n != 2 {
		return 0, 0, errors.New("expected HH:MM")
	}
	if hh < 0 || hh > 23 || mm < 0 || mm > 59 {
		return 0, 0, errors.New("HH or MM out of range")
	}
	return hh, mm, nil
}

// percentile uses the nearest-rank method: the sample at position
// ceil(p * n). sorted must already be sorted ascending.
func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}
