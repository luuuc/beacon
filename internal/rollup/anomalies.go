package rollup

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

// Anomaly fingerprint values encode the anomaly kind. These are stored in
// the Metric.Fingerprint field (which is part of the unique key) so that
// volume shifts and dimension spikes on the same metric produce distinct rows.
const (
	AnomalyVolumeShift  = "volume_shift"
	AnomalyDimensionSpike = "dimension_spike"
)

// shouldDetectAnomalies gates anomaly detection to once per calendar day,
// after the configured prune_at time — same cadence as pruning.
func (w *Worker) shouldDetectAnomalies(nowUTC time.Time) bool {
	local := nowUTC.In(w.cfg.Timezone)
	today := local.Format("2006-01-02")
	if today == w.lastAnomalyDate {
		return false
	}
	hh, mm, err := parseHHMM(w.cfg.PruneAt)
	if err != nil {
		return false
	}
	detectAt := time.Date(local.Year(), local.Month(), local.Day(), hh, mm, 0, 0, w.cfg.Timezone)
	if local.Before(detectAt) {
		return false
	}
	w.lastAnomalyDate = today
	return true
}

// detectAnomalies runs the sigma-threshold anomaly detector. It compares
// each metric's current detection window (default 24h) against the trailing
// baseline window (default 14d) and emits anomaly records for significant
// deviations.
func (w *Worker) detectAnomalies(ctx context.Context) error {
	now := w.now().UTC()
	cfg := w.cfg.Anomaly

	// Pull all hourly rollups within the baseline window. This includes
	// the current detection window (which is the tail of the baseline window).
	//
	// Expected scale: 14d × 24h × N metrics × M dimension slices. For a
	// solo-founder deployment (tens of endpoints, 2–3 dimensions), this is
	// a few thousand rows. Hourly retention is 90 days, so the 14-day
	// window is always a small subset. No pagination needed at this scale.
	baselineStart := now.Add(-cfg.BaselineWindow)
	detectionStart := now.Add(-cfg.DetectionWindow)

	hourlies, err := w.adapter.ListMetrics(ctx, beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodHour,
		Since:      baselineStart,
		Until:      now,
	})
	if err != nil {
		return fmt.Errorf("list hourlies for anomaly detection: %w", err)
	}
	if len(hourlies) == 0 {
		return nil
	}

	// Group hourly rows by (kind, name, dimension_hash).
	type groupKey struct {
		kind          beacondb.Kind
		name          string
		dimensionHash string
	}

	type metricGroup struct {
		baselineDailyCounts []int64      // one count per calendar day in baseline (excluding detection window)
		currentCount        int64        // sum of counts in detection window
		dimensions          map[string]any
	}

	groups := map[groupKey]*metricGroup{}

	for _, m := range hourlies {
		gk := groupKey{kind: m.Kind, name: m.Name, dimensionHash: m.DimensionHash}
		g, ok := groups[gk]
		if !ok {
			g = &metricGroup{dimensions: m.Dimensions}
			groups[gk] = g
		}

		if !m.PeriodStart.Before(detectionStart) {
			// In the detection window.
			g.currentCount += m.Count
		}
		// Baseline counts are accumulated per-day below.
	}

	// Compute per-day baseline counts (excluding the detection window).
	// We bucket hourly rows into calendar days and sum counts per group.
	type dayKey struct {
		group groupKey
		day   string // "2006-01-02"
	}
	dayCounts := map[dayKey]int64{}

	for _, m := range hourlies {
		// Only include hours strictly before the detection window in baseline.
		if !m.PeriodStart.Before(detectionStart) {
			continue
		}
		gk := groupKey{kind: m.Kind, name: m.Name, dimensionHash: m.DimensionHash}
		dk := dayKey{group: gk, day: m.PeriodStart.Format("2006-01-02")}
		dayCounts[dk] += m.Count
	}

	// Fold daily counts into per-group baseline arrays.
	for dk, count := range dayCounts {
		g, ok := groups[dk.group]
		if !ok {
			continue
		}
		g.baselineDailyCounts = append(g.baselineDailyCounts, count)
	}

	// Detect anomalies.
	detectionTime := now.Truncate(time.Hour)
	var anomalies []beacondb.Metric

	for gk, g := range groups {
		if g.currentCount < cfg.MinVolume {
			continue
		}
		if len(g.baselineDailyCounts) < 2 {
			// Not enough baseline data for meaningful stddev.
			continue
		}

		mean, stddev := meanStddev(g.baselineDailyCounts)
		if stddev == 0 {
			// Perfectly flat baseline — no deviation possible unless current
			// differs from mean, in which case any difference is infinite sigma.
			// Only flag if current actually differs from the baseline mean.
			if float64(g.currentCount) == mean {
				continue
			}
			// Treat as very high deviation when stddev is 0 but values differ.
			stddev = 1
		}

		deviation := (float64(g.currentCount) - mean) / stddev
		if deviation < cfg.SigmaThreshold {
			continue
		}

		// Determine anomaly kind.
		anomalyKind := AnomalyVolumeShift
		if gk.dimensionHash != "" {
			anomalyKind = AnomalyDimensionSpike
		}

		// Anomaly records reuse beacon_metrics fields with different semantics:
		//   Count         = current count in the detection window
		//   Sum           = deviation sigma (severity — higher means more anomalous)
		//   P50           = baseline daily mean
		//   P95           = baseline daily stddev
		//   Fingerprint   = anomaly kind ("volume_shift" or "dimension_spike")
		//   Dimensions    = the dimension slice (for dimension spikes)
		//   DimensionHash = dimension hash (for uniqueness in the index)
		anomalies = append(anomalies, beacondb.Metric{
			Kind:          gk.kind,
			Name:          gk.name,
			PeriodKind:    beacondb.PeriodAnomaly,
			PeriodWindow:  "24h",
			PeriodStart:   detectionTime,
			Count:         g.currentCount,
			Sum:           floatPtr(deviation),
			P50:           floatPtr(mean),
			P95:           floatPtr(stddev),
			Fingerprint:   anomalyKind,
			Dimensions:    g.dimensions,
			DimensionHash: gk.dimensionHash,
		})
	}

	if len(anomalies) == 0 {
		w.log.Info("anomaly detection complete",
			"event", "anomaly_detection_complete",
			"anomalies", 0,
		)
		return nil
	}

	if err := w.adapter.UpsertMetrics(ctx, anomalies); err != nil {
		return fmt.Errorf("upsert anomalies: %w", err)
	}
	w.log.Info("anomaly detection complete",
		"event", "anomaly_detection_complete",
		"anomalies", len(anomalies),
	)
	return nil
}

// meanStddev computes the population mean and standard deviation of daily counts.
func meanStddev(counts []int64) (mean, stddev float64) {
	if len(counts) == 0 {
		return 0, 0
	}
	var sum float64
	for _, c := range counts {
		sum += float64(c)
	}
	mean = sum / float64(len(counts))

	var variance float64
	for _, c := range counts {
		diff := float64(c) - mean
		variance += diff * diff
	}
	variance /= float64(len(counts))
	stddev = math.Sqrt(variance)
	return mean, stddev
}

func floatPtr(v float64) *float64 { return &v }
