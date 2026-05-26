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
// different anomaly types on the same metric produce distinct rows.
const (
	AnomalyVolumeShift    = "volume_shift"
	AnomalyDimensionSpike = "dimension_spike"
	AnomalyPerfDrift      = "perf_drift"
	AnomalyErrorRateSpike = "error_rate_spike"
	AnomalyOutcomeDrop    = "outcome_drop"
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
		baselineDailyCounts []int64        // one count per calendar day in baseline (excluding detection window)
		currentCount        int64          // sum of counts in detection window
		baselineDailyP95s   []float64      // one max-p95 per calendar day in baseline (perf only)
		currentP95s         []float64      // p95 values from detection window hours (perf only)
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
			g.currentCount += m.Count
			if m.P95 != nil {
				g.currentP95s = append(g.currentP95s, *m.P95)
			}
		}
	}

	// Compute per-day baseline counts and p95 maxima (excluding the detection window).
	type dayKey struct {
		group groupKey
		day   string // "2006-01-02"
	}
	dayCounts := map[dayKey]int64{}
	dayP95Max := map[dayKey]float64{}

	for _, m := range hourlies {
		if !m.PeriodStart.Before(detectionStart) {
			continue
		}
		gk := groupKey{kind: m.Kind, name: m.Name, dimensionHash: m.DimensionHash}
		dk := dayKey{group: gk, day: m.PeriodStart.Format("2006-01-02")}
		dayCounts[dk] += m.Count
		if m.P95 != nil {
			if v, exists := dayP95Max[dk]; !exists || *m.P95 > v {
				dayP95Max[dk] = *m.P95
			}
		}
	}

	// Fold daily counts and p95s into per-group baseline arrays.
	for dk, count := range dayCounts {
		g, ok := groups[dk.group]
		if !ok {
			continue
		}
		g.baselineDailyCounts = append(g.baselineDailyCounts, count)
	}
	for dk, p95 := range dayP95Max {
		g, ok := groups[dk.group]
		if !ok {
			continue
		}
		g.baselineDailyP95s = append(g.baselineDailyP95s, p95)
	}

	// Detect anomalies.
	detectionTime := now.Truncate(time.Hour)
	var anomalies []beacondb.Metric

	for gk, g := range groups {
		// --- Volume-based detection (volume_shift, dimension_spike, error_rate_spike, outcome_drop) ---
		if g.currentCount >= cfg.MinVolume && len(g.baselineDailyCounts) >= 2 {
			mean, stddev := meanStddev(g.baselineDailyCounts)
			if stddev == 0 {
				if float64(g.currentCount) != mean {
					stddev = 1
				}
			}

			if stddev > 0 {
				deviation := (float64(g.currentCount) - mean) / stddev

				if gk.kind == beacondb.KindOutcome && deviation < -cfg.SigmaThreshold {
					// Outcome drop: conversion/success rate fell significantly.
					anomalies = append(anomalies, beacondb.Metric{
						Kind:          gk.kind,
						Name:          gk.name,
						PeriodKind:    beacondb.PeriodAnomaly,
						PeriodWindow:  "24h",
						PeriodStart:   detectionTime,
						Count:         g.currentCount,
						Sum:           floatPtr(-deviation),
						P50:           floatPtr(mean),
						P95:           floatPtr(stddev),
						Fingerprint:   AnomalyOutcomeDrop,
						Dimensions:    g.dimensions,
						DimensionHash: gk.dimensionHash,
					})
				} else if deviation >= cfg.SigmaThreshold {
					anomalyKind := AnomalyVolumeShift
					if gk.kind == beacondb.KindError {
						anomalyKind = AnomalyErrorRateSpike
					} else if gk.dimensionHash != "" {
						anomalyKind = AnomalyDimensionSpike
					}

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
			}
		}

		// --- Latency-based detection (perf_drift) ---
		if gk.kind == beacondb.KindPerf && gk.dimensionHash == "" &&
			len(g.currentP95s) > 0 && len(g.baselineDailyP95s) >= 2 {

			currentP95 := maxFloat(g.currentP95s)
			mean, stddev := meanStddevFloat(g.baselineDailyP95s)
			if stddev == 0 && currentP95 != mean {
				stddev = 1
			}
			if stddev > 0 {
				deviation := (currentP95 - mean) / stddev
				if deviation >= cfg.SigmaThreshold {
					anomalies = append(anomalies, beacondb.Metric{
						Kind:         gk.kind,
						Name:         gk.name,
						PeriodKind:   beacondb.PeriodAnomaly,
						PeriodWindow: "24h",
						PeriodStart:  detectionTime,
						Count:        g.currentCount,
						Sum:          floatPtr(deviation),
						P50:          floatPtr(mean),
						P95:          floatPtr(currentP95),
						P99:          floatPtr(stddev),
						Fingerprint:  AnomalyPerfDrift,
					})
				}
			}
		}
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

// meanStddevFloat computes population mean and standard deviation of float64 slices.
func meanStddevFloat(vals []float64) (mean, stddev float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	mean = sum / float64(len(vals))

	var variance float64
	for _, v := range vals {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(vals))
	stddev = math.Sqrt(variance)
	return mean, stddev
}

func maxFloat(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	m := vals[0]
	for _, v := range vals[1:] {
		if v > m {
			m = v
		}
	}
	return m
}

func floatPtr(v float64) *float64 { return &v }
