package rollup

import (
	"context"
	"fmt"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

// Verdict is the three-level result of comparing a current window to a
// baseline. Patrol (and anyone else wiring beacon into CI) consumes these.
type Verdict string

const (
	// VerdictPass — current is within ±20% of baseline.
	VerdictPass Verdict = "pass"
	// VerdictDrift — current is within ±50% but outside ±20%. Worth a look.
	VerdictDrift Verdict = "drift"
	// VerdictFail — current is more than 50% off baseline. Actionable.
	VerdictFail Verdict = "fail"
	// VerdictInsufficient — there isn't enough data on one side to judge.
	VerdictInsufficient Verdict = "insufficient"
)

// CompareCount returns a verdict for a count-only metric (outcomes/errors).
// Both numbers should already be normalized to the same window size; this
// function does not scale by elapsed time.
//
//   - baseline = 0 and current = 0 → pass (nothing happened, nothing changed)
//   - baseline = 0 and current > 0 → fail (a new signal appeared)
//   - baseline > 0                 → ratio-based bands
func CompareCount(current, baseline int64) Verdict {
	if baseline == 0 {
		if current == 0 {
			return VerdictPass
		}
		return VerdictFail
	}
	ratio := float64(current) / float64(baseline)
	switch {
	case ratio >= 0.8 && ratio <= 1.2:
		return VerdictPass
	case ratio >= 0.5 && ratio <= 1.5:
		return VerdictDrift
	default:
		return VerdictFail
	}
}

// Comparison is the full shape returned by CompareDeployBaseline. It carries
// the counts and ratio alongside the verdict so the caller can render a
// human-readable summary.
type Comparison struct {
	Metric       string    `json:"metric"`
	Fingerprint  string    `json:"fingerprint,omitempty"`
	BaselineTime time.Time `json:"baseline_time"`
	Baseline     int64     `json:"baseline"`
	Current      int64     `json:"current"`
	Ratio        float64   `json:"ratio"`
	Verdict      Verdict   `json:"verdict"`
}

// CompareDeployBaseline reads the deployment baseline captured at deployTime
// for (kind, name) and compares it to the current window's counts (defined
// as the metrics since deployTime). Both sides are count totals over
// equal-length windows: the baseline covers [deploy-24h, deploy); the
// current window covers [deploy, now). If now-deploy is shorter than the
// lookback, the current count is scaled up to match 24h.
//
// This is the v1 primitive the `deploy → baseline → compare` flow hangs on.
// More sophisticated comparison (per-endpoint p95 drift, etc.) can live in
// their own helpers without touching this one.
func (w *Worker) CompareDeployBaseline(ctx context.Context, kind beacondb.Kind, name string, deployTime time.Time) (Comparison, error) {
	// Truncate to second precision to match the capture side (baselines.go)
	// and to tolerate pre-fix baselines that may still carry sub-seconds.
	deployTime = deployTime.Truncate(time.Second)
	cmp := Comparison{Metric: name, BaselineTime: deployTime}

	baselines, err := w.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:         kind,
		Name:         name,
		PeriodKind:   beacondb.PeriodBaseline,
		PeriodWindow: DeploymentBaselineWindow,
	})
	if err != nil {
		return cmp, fmt.Errorf("read deploy baselines: %w", err)
	}
	var baseline *beacondb.Metric
	for i := range baselines {
		if baselines[i].PeriodStart.Truncate(time.Second).Equal(deployTime) {
			baseline = &baselines[i]
			break
		}
	}
	if baseline == nil {
		cmp.Verdict = VerdictInsufficient
		return cmp, nil
	}
	cmp.Baseline = baseline.Count
	cmp.Fingerprint = baseline.Fingerprint

	// Current window: hourly metric rows since deployTime.
	now := w.now().UTC()
	currentHourlies, err := w.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		Kind:       kind,
		Name:       name,
		PeriodKind: beacondb.PeriodHour,
		Since:      deployTime,
		Until:      now.Add(time.Hour), // include the current hour
	})
	if err != nil {
		return cmp, fmt.Errorf("read current hourlies: %w", err)
	}
	var currentTotal int64
	for _, m := range currentHourlies {
		currentTotal += m.Count
	}

	// Normalize the current window to the same 24h shape as the baseline so
	// ratios compare like-for-like. If less than 24h has elapsed, scale up.
	elapsed := now.Sub(deployTime)
	if elapsed <= 0 {
		cmp.Current = currentTotal
	} else if elapsed < DeploymentLookback {
		cmp.Current = int64(float64(currentTotal) * (float64(DeploymentLookback) / float64(elapsed)))
	} else {
		cmp.Current = currentTotal
	}

	if cmp.Baseline > 0 {
		cmp.Ratio = float64(cmp.Current) / float64(cmp.Baseline)
	}
	cmp.Verdict = CompareCount(cmp.Current, cmp.Baseline)
	return cmp, nil
}
