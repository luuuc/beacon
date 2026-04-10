package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

// DeployEventName is the outcome event that triggers a deployment baseline
// snapshot. Clients send this from CI on successful deploy.
const DeployEventName = "deploy.shipped"

// Trailing baseline windows. Order matters: the longest is used to bound
// the query that finds active metrics.
var trailingWindows = []struct {
	label    string
	duration time.Duration
}{
	{"24h", 24 * time.Hour},
	{"7d", 7 * 24 * time.Hour},
	{"30d", 30 * 24 * time.Hour},
}

// DeploymentBaselineWindow is the period_window label used for deployment
// baselines. It distinguishes them from trailing baselines (which carry
// 24h/7d/30d) and from hourly rollups. The unique index on beacon_metrics
// ensures one deployment baseline row per deploy timestamp.
const DeploymentBaselineWindow = "deploy"

// DeploymentLookback is the window summarized in a deployment baseline —
// "what the metric looked like in the 24h before the deploy fired".
const DeploymentLookback = 24 * time.Hour

// aggregateTrailingBaselines writes 24h/7d/30d baseline rows for every
// metric that has any hourly rollups inside the longest window. Run on
// every tick; the upsert key (kind, name, fingerprint, period_window,
// period_start) means re-runs inside the same hour overwrite cleanly.
func (w *Worker) aggregateTrailingBaselines(ctx context.Context) error {
	now := w.now().UTC()
	longest := trailingWindows[len(trailingWindows)-1].duration

	hourlies, err := w.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodHour,
		Since:      now.Add(-longest),
		Until:      now,
	})
	if err != nil {
		return fmt.Errorf("list hourlies: %w", err)
	}
	if len(hourlies) == 0 {
		return nil
	}

	// period_start is the top of the current hour so repeated ticks inside
	// the same hour upsert the same row (idempotent).
	baselineStart := now.Truncate(time.Hour)

	var toUpsert []beacondb.Metric
	for _, win := range trailingWindows {
		cutoff := now.Add(-win.duration)
		inWindow := make([]beacondb.Metric, 0, len(hourlies))
		for _, m := range hourlies {
			if m.PeriodStart.Before(cutoff) {
				continue
			}
			inWindow = append(inWindow, m)
		}
		baselines := aggregateHourliesIntoBaselines(inWindow, baselineStart, win.label)
		toUpsert = append(toUpsert, baselines...)
	}

	if len(toUpsert) == 0 {
		return nil
	}
	return w.adapter.UpsertMetrics(ctx, toUpsert)
}

// captureDeploymentBaselines scans recent deploy.shipped events and writes
// one deployment baseline row per (deploy, metric). Upsert semantics make
// this safe to re-run: the same deploy + same metric produces the same row.
//
// The scan window is 7 days: older deploys are out of v1's interest
// horizon, and a deploy landing more than 7 days after the worker was
// running means the operator probably wants a manual recompute anyway.
func (w *Worker) captureDeploymentBaselines(ctx context.Context) error {
	now := w.now().UTC()
	deploys, err := w.adapter.ListEvents(ctx, beacondb.EventFilter{
		Kind:  beacondb.KindOutcome,
		Name:  DeployEventName,
		Since: now.Add(-7 * 24 * time.Hour),
		Until: now,
	})
	if err != nil {
		return fmt.Errorf("list deploys: %w", err)
	}
	for _, deploy := range deploys {
		if err := w.captureDeploymentBaselineAt(ctx, deploy.CreatedAt); err != nil {
			return fmt.Errorf("capture deploy baseline at %s: %w", deploy.CreatedAt.Format(time.RFC3339), err)
		}
	}
	return nil
}

// captureDeploymentBaselineAt snapshots the 24h-before window for every
// metric active in that window.
func (w *Worker) captureDeploymentBaselineAt(ctx context.Context, deployTime time.Time) error {
	lookbackStart := deployTime.Add(-DeploymentLookback)
	hourlies, err := w.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodHour,
		Since:      lookbackStart,
		Until:      deployTime,
	})
	if err != nil {
		return err
	}
	baselines := aggregateHourliesIntoBaselines(hourlies, deployTime, DeploymentBaselineWindow)
	if len(baselines) == 0 {
		return nil
	}
	return w.adapter.UpsertMetrics(ctx, baselines)
}

// aggregateHourliesIntoBaselines folds a set of hourly metric rows into one
// baseline row per (kind, name, fingerprint). Only count and sum survive
// the fold: percentiles are deliberately dropped because a "baseline p95"
// that is the average of 24 or 720 hourly p95 values is not a percentile of
// anything — it cannot be honestly recovered without the raw durations,
// which retention has already pruned. Readers that want a baseline
// percentile should compare current windows against the trailing windows
// directly, as GetPerfEndpoints does.
func aggregateHourliesIntoBaselines(hourlies []beacondb.Metric, periodStart time.Time, periodWindow string) []beacondb.Metric {
	type groupKey struct {
		kind        beacondb.Kind
		name        string
		fingerprint string
	}

	type accum struct {
		count  int64
		sum    float64
		hasSum bool
	}

	groups := map[groupKey]*accum{}
	for _, m := range hourlies {
		gk := groupKey{kind: m.Kind, name: m.Name, fingerprint: m.Fingerprint}
		a, ok := groups[gk]
		if !ok {
			a = &accum{}
			groups[gk] = a
		}
		a.count += m.Count
		if m.Sum != nil {
			a.sum += *m.Sum
			a.hasSum = true
		}
	}

	out := make([]beacondb.Metric, 0, len(groups))
	for gk, a := range groups {
		if a.count == 0 {
			continue
		}
		bm := beacondb.Metric{
			Kind:          gk.kind,
			Name:          gk.name,
			Fingerprint:   gk.fingerprint,
			PeriodKind:    beacondb.PeriodBaseline,
			PeriodWindow:  periodWindow,
			PeriodStart:   periodStart,
			Count:         a.count,
			DimensionHash: "",
		}
		if a.hasSum {
			s := a.sum
			bm.Sum = &s
		}
		out = append(out, bm)
	}
	return out
}

// RecomputeRange re-derives every hour bucket touched by events in [since, now].
// Used by `beacon rollup recompute`. Kind/name, when non-empty, narrow the
// event scan to reduce the set of touched buckets — aggregateHour re-reads
// every event in each touched bucket regardless, so the flags are an
// optimization hint, not a filter on what gets overwritten.
func (w *Worker) RecomputeRange(ctx context.Context, since time.Time, kind beacondb.Kind, name string) error {
	now := w.now().UTC()
	events, err := w.adapter.ListEvents(ctx, beacondb.EventFilter{
		Kind:  kind,
		Name:  name,
		Since: since,
		Until: now,
	})
	if err != nil {
		return fmt.Errorf("list events: %w", err)
	}
	touched := map[time.Time]bool{}
	for _, e := range events {
		touched[e.CreatedAt.Truncate(time.Hour)] = true
	}
	for h := range touched {
		if err := w.aggregateHour(ctx, h); err != nil {
			return fmt.Errorf("aggregate hour %s: %w", h.Format(time.RFC3339), err)
		}
	}
	// Refresh trailing baselines so the restored state is consistent.
	if err := w.aggregateTrailingBaselines(ctx); err != nil {
		return fmt.Errorf("refresh trailing baselines: %w", err)
	}
	return nil
}
