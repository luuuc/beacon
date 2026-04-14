package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
)

// seedHourlyMetrics seeds hourly rollup rows directly into the metrics table.
// This simulates the output of aggregateHour without needing to seed raw events
// and run the full rollup pipeline — the anomaly detector operates on metrics,
// not events.
func seedHourlyMetrics(t *testing.T, fake *memfake.Fake, metrics ...beacondb.Metric) {
	t.Helper()
	if err := fake.UpsertMetrics(context.Background(), metrics); err != nil {
		t.Fatalf("seed metrics: %v", err)
	}
}

// makeHourlyRow builds a minimal hourly metric for a given day offset and count.
func makeHourlyRow(kind beacondb.Kind, name string, dayOffset int, count int64, baseTime time.Time, dims map[string]any) beacondb.Metric {
	dh, _ := beacondb.DimensionHash(dims)
	return beacondb.Metric{
		Kind:          kind,
		Name:          name,
		PeriodKind:    beacondb.PeriodHour,
		PeriodWindow:  "hour",
		PeriodStart:   baseTime.Add(time.Duration(dayOffset) * 24 * time.Hour),
		Count:         count,
		Dimensions:    dims,
		DimensionHash: dh,
	}
}

func newAnomalyWorker(t *testing.T, now time.Time, cfg AnomalyConfig) (*Worker, *memfake.Fake) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{
		TickInterval: time.Minute,
		RetentionRaw: 14 * 24 * time.Hour,
		Anomaly:      cfg,
	}, fake, nil)
	w.now = func() time.Time { return now }
	return w, fake
}

// ---------------------------------------------------------------------------
// Conformance fixture: volume shift
// ---------------------------------------------------------------------------

func TestAnomaly_volumeShift(t *testing.T) {
	// Detection time: day 14. Baseline: days 0-12 (~10 events/day).
	// Day 13 (detection window): 100 events — a clear spike.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC) // after prune_at
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       5,
	})

	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric

	// Baseline: 13 days of ~10 events/day (one hourly row per day for simplicity).
	for d := 0; d < 13; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "http_request", d, 10, baseTime, nil))
	}
	// Detection window (day 13): spike to 100.
	metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "http_request", 13, 100, baseTime, nil))

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 1 {
		t.Fatalf("anomalies = %d, want 1", len(anomalies))
	}
	a := anomalies[0]
	if a.Kind != beacondb.KindAmbient {
		t.Errorf("kind = %q, want ambient", a.Kind)
	}
	if a.Name != "http_request" {
		t.Errorf("name = %q, want http_request", a.Name)
	}
	if a.Fingerprint != AnomalyVolumeShift {
		t.Errorf("fingerprint = %q, want %q", a.Fingerprint, AnomalyVolumeShift)
	}
	if a.Count != 100 {
		t.Errorf("count (current) = %d, want 100", a.Count)
	}
	if a.Sum == nil || *a.Sum < 2.0 {
		t.Errorf("sum (deviation sigma) = %v, want >= 2.0", a.Sum)
	}
	if a.DimensionHash != "" {
		t.Errorf("dimension_hash should be empty for volume shift")
	}
}

// ---------------------------------------------------------------------------
// Conformance fixture: dimension spike
// ---------------------------------------------------------------------------

func TestAnomaly_dimensionSpike(t *testing.T) {
	// Baseline: 13 days of ~5 US events/day.
	// Detection window: 50 US events — spike.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       5,
	})

	dims := map[string]any{"country": "US"}
	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric

	for d := 0; d < 13; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "signup.completed", d, 5, baseTime, dims))
	}
	// Spike in detection window.
	metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "signup.completed", 13, 50, baseTime, dims))

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 1 {
		t.Fatalf("anomalies = %d, want 1", len(anomalies))
	}
	a := anomalies[0]
	if a.Fingerprint != AnomalyDimensionSpike {
		t.Errorf("fingerprint = %q, want %q", a.Fingerprint, AnomalyDimensionSpike)
	}
	if a.DimensionHash == "" {
		t.Error("dimension_hash should be non-empty for dimension spike")
	}
	if a.Dimensions["country"] != "US" {
		t.Errorf("dimensions = %+v, want {country: US}", a.Dimensions)
	}
}

// ---------------------------------------------------------------------------
// Conformance fixture: below-min-volume suppression
// ---------------------------------------------------------------------------

func TestAnomaly_belowMinVolumeSuppressed(t *testing.T) {
	// Baseline: 13 days of 1 event/day. Detection: 5 events.
	// With min_volume=10, the anomaly should be suppressed.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       10,
	})

	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric
	for d := 0; d < 13; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "low_traffic", d, 1, baseTime, nil))
	}
	metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "low_traffic", 13, 5, baseTime, nil))

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 0 {
		t.Errorf("anomalies = %d, want 0 (below min_volume)", len(anomalies))
	}
}

// ---------------------------------------------------------------------------
// Conformance fixture: no-anomaly baseline (steady traffic)
// ---------------------------------------------------------------------------

func TestAnomaly_noAnomalyBaseline(t *testing.T) {
	// All 14 days have ~10 events/day. Detection window is also ~10.
	// No anomaly should fire.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       5,
	})

	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric
	for d := 0; d < 14; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "steady", d, 10, baseTime, nil))
	}

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 0 {
		t.Errorf("anomalies = %d, want 0 (steady traffic)", len(anomalies))
	}
}

// ---------------------------------------------------------------------------
// Conformance fixture: cold-start bootstrap (perf baselines, zero ambient)
// ---------------------------------------------------------------------------

func TestAnomaly_coldStartBootstrap(t *testing.T) {
	// 13 days of perf baselines for "GET /search" at ~20/day.
	// Detection window: perf spike to 200.
	// No ambient data exists. The detector should fire on the perf data.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       5,
	})

	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric
	for d := 0; d < 13; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindPerf, "GET /search", d, 20, baseTime, nil))
	}
	// Spike in detection window.
	metrics = append(metrics, makeHourlyRow(beacondb.KindPerf, "GET /search", 13, 200, baseTime, nil))

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 1 {
		t.Fatalf("anomalies = %d, want 1 (cold-start perf spike)", len(anomalies))
	}
	a := anomalies[0]
	if a.Kind != beacondb.KindPerf {
		t.Errorf("kind = %q, want perf", a.Kind)
	}
	if a.Name != "GET /search" {
		t.Errorf("name = %q, want GET /search", a.Name)
	}
}

// ---------------------------------------------------------------------------
// Conformance fixture: downward deviation intentionally suppressed
// ---------------------------------------------------------------------------

func TestAnomaly_downwardDeviationSuppressed(t *testing.T) {
	// Baseline: 13 days of 100 events/day. Detection: 5 events (traffic drop).
	// The detector only flags upward deviations (current > baseline), so a
	// traffic drop should not fire an anomaly.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	w, fake := newAnomalyWorker(t, now, AnomalyConfig{
		BaselineWindow:  14 * 24 * time.Hour,
		DetectionWindow: 24 * time.Hour,
		SigmaThreshold:  2.0,
		MinVolume:       5,
	})

	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)
	var metrics []beacondb.Metric
	for d := 0; d < 13; d++ {
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "dropping", d, 100, baseTime, nil))
	}
	// Detection window: traffic dropped to 5.
	metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "dropping", 13, 5, baseTime, nil))

	seedHourlyMetrics(t, fake, metrics...)

	if err := w.detectAnomalies(context.Background()); err != nil {
		t.Fatal(err)
	}

	anomalies, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodAnomaly,
	})
	if len(anomalies) != 0 {
		t.Errorf("anomalies = %d, want 0 (downward deviation intentionally suppressed)", len(anomalies))
	}
}

// ---------------------------------------------------------------------------
// Default 3σ threshold
// ---------------------------------------------------------------------------

func TestAnomaly_default3SigmaThreshold(t *testing.T) {
	// Use zero-value AnomalyConfig so withDefaults() fills in the 3.0 default.
	now := time.Date(2026, 4, 14, 4, 0, 0, 0, time.UTC)
	baseTime := now.Add(-14 * 24 * time.Hour).Truncate(time.Hour)

	t.Run("2.5sigma suppressed", func(t *testing.T) {
		fake2 := memfake.New()
		if err := fake2.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		w2 := NewWorker(Config{
			TickInterval: time.Minute,
			RetentionRaw: 14 * 24 * time.Hour,
		}, fake2, nil)
		w2.now = func() time.Time { return now }

		// Baseline with variance: days with counts [8,12,8,12,...] → mean≈10, stddev≈2.
		// Detection window: 15 events → deviation=(15-10)/2=2.5σ → below 3σ.
		var metrics []beacondb.Metric
		for d := 0; d < 13; d++ {
			count := int64(8)
			if d%2 == 0 {
				count = 12
			}
			metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "moderate_spike", d, count, baseTime, nil))
		}
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "moderate_spike", 13, 15, baseTime, nil))
		seedHourlyMetrics(t, fake2, metrics...)

		if err := w2.detectAnomalies(context.Background()); err != nil {
			t.Fatal(err)
		}
		anomalies, _ := fake2.ListMetrics(context.Background(), beacondb.MetricFilter{
			PeriodKind: beacondb.PeriodAnomaly,
		})
		if len(anomalies) != 0 {
			t.Errorf("anomalies = %d, want 0 (2.5σ below default 3σ threshold)", len(anomalies))
		}
	})

	t.Run("3.5sigma fires", func(t *testing.T) {
		fake3 := memfake.New()
		if err := fake3.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		w3 := NewWorker(Config{
			TickInterval: time.Minute,
			RetentionRaw: 14 * 24 * time.Hour,
		}, fake3, nil)
		w3.now = func() time.Time { return now }

		// Baseline with variance: [8,12,8,12,...] → mean≈10, stddev≈2.
		// Detection window: 17 events → deviation=(17-10)/2=3.5σ → above 3σ.
		var metrics []beacondb.Metric
		for d := 0; d < 13; d++ {
			count := int64(8)
			if d%2 == 0 {
				count = 12
			}
			metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "big_spike", d, count, baseTime, nil))
		}
		metrics = append(metrics, makeHourlyRow(beacondb.KindAmbient, "big_spike", 13, 17, baseTime, nil))
		seedHourlyMetrics(t, fake3, metrics...)

		if err := w3.detectAnomalies(context.Background()); err != nil {
			t.Fatal(err)
		}
		anomalies, _ := fake3.ListMetrics(context.Background(), beacondb.MetricFilter{
			PeriodKind: beacondb.PeriodAnomaly,
		})
		if len(anomalies) != 1 {
			t.Fatalf("anomalies = %d, want 1 (3.5σ above default 3σ threshold)", len(anomalies))
		}
	})
}

// ---------------------------------------------------------------------------
// meanStddev unit tests
// ---------------------------------------------------------------------------

func TestMeanStddev(t *testing.T) {
	cases := []struct {
		name       string
		counts     []int64
		wantMean   float64
		wantStddev float64
	}{
		{"empty", nil, 0, 0},
		{"single", []int64{10}, 10, 0},
		{"uniform", []int64{5, 5, 5, 5}, 5, 0},
		{"simple", []int64{10, 20}, 15, 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mean, stddev := meanStddev(tc.counts)
			if mean != tc.wantMean {
				t.Errorf("mean = %v, want %v", mean, tc.wantMean)
			}
			if diff := stddev - tc.wantStddev; diff > 0.001 || diff < -0.001 {
				t.Errorf("stddev = %v, want %v", stddev, tc.wantStddev)
			}
		})
	}
}
