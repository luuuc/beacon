package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

// ---------------------------------------------------------------------------
// Trailing baselines
// ---------------------------------------------------------------------------

func TestTrailingBaselines_threeWindowsPerMetric(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup.completed", CreatedAt: hour.Add(time.Minute)},
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup.completed", CreatedAt: hour.Add(2 * time.Minute)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "signup.completed", PeriodKind: beacondb.PeriodBaseline,
	})
	if len(rows) != 3 {
		t.Fatalf("baseline rows = %d, want 3 (24h/7d/30d)", len(rows))
	}
	got := map[string]int64{}
	for _, r := range rows {
		got[r.PeriodWindow] = r.Count
		if !r.PeriodStart.Equal(hour) {
			t.Errorf("%s period_start = %s, want %s", r.PeriodWindow, r.PeriodStart, hour)
		}
	}
	for _, w := range []string{"24h", "7d", "30d"} {
		if got[w] != 2 {
			t.Errorf("baseline[%s] count = %d, want 2", w, got[w])
		}
	}
}

func TestTrailingBaselines_shorterWindowsExcludeOlderHours(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	thisHour := fixedNow.Truncate(time.Hour)
	prevHour := thisHour.Add(-time.Hour)
	dayAgo := thisHour.Add(-25 * time.Hour)

	// Seed hourly rollups directly (skip aggregation by writing metrics) so
	// we can test the trailing-baseline filter without depending on raw events
	// that would also get aggregated this tick.
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: thisHour, Count: 10},
		{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: prevHour, Count: 5},
		{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: dayAgo, Count: 100},
	})

	if err := w.aggregateTrailingBaselines(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodBaseline,
	})
	got := map[string]int64{}
	for _, r := range rows {
		got[r.PeriodWindow] = r.Count
	}
	// 24h window: excludes the 25h-ago row → 10 + 5 = 15
	// 7d window: includes everything → 115
	// 30d window: includes everything → 115
	if got["24h"] != 15 {
		t.Errorf("24h = %d, want 15", got["24h"])
	}
	if got["7d"] != 115 {
		t.Errorf("7d = %d, want 115", got["7d"])
	}
	if got["30d"] != 115 {
		t.Errorf("30d = %d, want 115", got["30d"])
	}
}

func TestTrailingBaselines_perfAveragesPercentiles(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	thisHour := fixedNow.Truncate(time.Hour)
	prevHour := thisHour.Add(-time.Hour)

	p50a, p95a, p99a := 40.0, 90.0, 99.0
	p50b, p95b, p99b := 60.0, 110.0, 121.0
	sumA, sumB := 1000.0, 2000.0
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindPerf, Name: "GET /", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: thisHour, Count: 10, Sum: &sumA, P50: &p50a, P95: &p95a, P99: &p99a},
		{Kind: beacondb.KindPerf, Name: "GET /", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: prevHour, Count: 20, Sum: &sumB, P50: &p50b, P95: &p95b, P99: &p99b},
	})

	if err := w.aggregateTrailingBaselines(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindPerf, Name: "GET /", PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "24h",
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	b := rows[0]
	if b.Count != 30 {
		t.Errorf("count = %d, want 30", b.Count)
	}
	if b.Sum == nil || *b.Sum != 3000 {
		t.Errorf("sum = %v, want 3000", b.Sum)
	}
	if b.P50 == nil || *b.P50 != 50.0 {
		t.Errorf("p50 = %v, want 50", b.P50)
	}
	if b.P95 == nil || *b.P95 != 100.0 {
		t.Errorf("p95 = %v, want 100", b.P95)
	}
	if b.P99 == nil || *b.P99 != 110.0 {
		t.Errorf("p99 = %v, want 110", b.P99)
	}
	_ = w // silence
}

// ---------------------------------------------------------------------------
// Deployment baselines
// ---------------------------------------------------------------------------

func TestDeploymentBaseline_capturedOnDeployEvent(t *testing.T) {
	// 12:30 is the current time. The deploy fired at 12:00. The 24h lookback
	// should include everything between 11:30 yesterday and 12:00 today — and
	// in this test, that's the two hourly rollups at 10:00 and 11:00.
	w, fake := newTestWorker(t, fixedNow)
	deployTime := fixedNow.Truncate(time.Hour) // 12:00

	// Pre-deploy hourlies.
	hourMinus2 := deployTime.Add(-2 * time.Hour) // 10:00
	hourMinus1 := deployTime.Add(-1 * time.Hour) // 11:00
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup.completed", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: hourMinus2, Count: 30},
		{Kind: beacondb.KindOutcome, Name: "signup.completed", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: hourMinus1, Count: 40},
	})
	// The deploy event itself.
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: DeployEventName, CreatedAt: deployTime},
	)

	if err := w.captureDeploymentBaselines(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "signup.completed",
		PeriodKind: beacondb.PeriodBaseline, PeriodWindow: DeploymentBaselineWindow,
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Count != 70 {
		t.Errorf("count = %d, want 70", rows[0].Count)
	}
	if !rows[0].PeriodStart.Equal(deployTime) {
		t.Errorf("period_start = %s, want %s", rows[0].PeriodStart, deployTime)
	}
}

func TestDeploymentBaseline_idempotent(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	deployTime := fixedNow.Truncate(time.Hour)
	_ = fake.UpsertMetrics(context.Background(), []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: deployTime.Add(-time.Hour), Count: 50},
	})
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: DeployEventName, CreatedAt: deployTime},
	)

	for i := 0; i < 3; i++ {
		if err := w.captureDeploymentBaselines(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "x",
		PeriodKind: beacondb.PeriodBaseline, PeriodWindow: DeploymentBaselineWindow,
	})
	if len(rows) != 1 {
		t.Errorf("re-running produced duplicates: %d rows", len(rows))
	}
}

// ---------------------------------------------------------------------------
// CompareCount + CompareDeployBaseline
// ---------------------------------------------------------------------------

func TestCompareCount(t *testing.T) {
	cases := []struct {
		name     string
		current  int64
		baseline int64
		want     Verdict
	}{
		{"both zero → pass", 0, 0, VerdictPass},
		{"new signal vs empty baseline → fail", 10, 0, VerdictFail},
		{"exact match → pass", 100, 100, VerdictPass},
		{"5% off → pass", 105, 100, VerdictPass},
		{"20% low → pass", 80, 100, VerdictPass},
		{"30% low → drift", 70, 100, VerdictDrift},
		{"50% low → drift (boundary)", 50, 100, VerdictDrift},
		{"60% low → fail", 40, 100, VerdictFail},
		{"50% high → drift", 150, 100, VerdictDrift},
		{"2x high → fail", 200, 100, VerdictFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := CompareCount(tc.current, tc.baseline); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// TestDeployToCompareFlow is the card's done-gate: seed pre-deploy events,
// fire a deploy, let the worker capture the baseline, seed post-deploy
// events that are roughly on par with the baseline, then assert the compare
// returns a pass verdict.
func TestDeployToCompareFlow(t *testing.T) {
	// Timeline: deploy at 12:00. Pre-deploy window is [11:00, 12:00) with
	// 100 events. Post-deploy we seed 100 events between 12:00 and 12:30
	// (half the window elapsed) — after normalization the current-window
	// projection is ~200, which is fail territory, not pass. To get a pass
	// we need ~50 events in 30 minutes (projected to 100 over 60 minutes).
	//
	// The normalization is scale-up to 24h, but here the baseline lookback
	// is 24h and "current" is only a fraction of that — so the scale-up is
	// aggressive. To exercise pass cleanly, we set "now" to exactly 24h
	// after the deploy so normalization is a no-op.

	deployTime := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	now := deployTime.Add(24 * time.Hour)

	w, fake := newTestWorker(t, now)

	// Seed pre-deploy events: 100 outcomes in the 24h before the deploy.
	// We spread them across the last 24 hours to produce 24 hourly rollups.
	for h := 0; h < 24; h++ {
		for i := 0; i < 100/24; i++ {
			seedEvents(t, fake, beacondb.Event{
				Kind:      beacondb.KindOutcome,
				Name:      "signup.completed",
				CreatedAt: deployTime.Add(-time.Duration(24-h)*time.Hour + time.Duration(i)*time.Minute),
			})
		}
	}
	// Deploy event.
	seedEvents(t, fake, beacondb.Event{
		Kind: beacondb.KindOutcome, Name: DeployEventName, CreatedAt: deployTime,
	})
	// Post-deploy events: 96 over the 24h after. Spread across 24 hours.
	for h := 0; h < 24; h++ {
		for i := 0; i < 4; i++ {
			seedEvents(t, fake, beacondb.Event{
				Kind:      beacondb.KindOutcome,
				Name:      "signup.completed",
				CreatedAt: deployTime.Add(time.Duration(h)*time.Hour + time.Duration(i)*time.Minute),
			})
		}
	}

	// We need hourly rollups to exist for all 48 hours. Call RecomputeRange
	// to derive every hourly bucket from raw events.
	if err := w.RecomputeRange(context.Background(), deployTime.Add(-24*time.Hour), "", ""); err != nil {
		t.Fatalf("RecomputeRange: %v", err)
	}
	// Then run a full tick which also fires captureDeploymentBaselines.
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// Sanity: baseline row exists.
	baselines, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "signup.completed",
		PeriodKind: beacondb.PeriodBaseline, PeriodWindow: DeploymentBaselineWindow,
	})
	if len(baselines) != 1 {
		t.Fatalf("deploy baseline rows = %d", len(baselines))
	}
	if baselines[0].Count != 96 { // 24 * (100/24=4) = 96
		t.Errorf("baseline count = %d, want 96", baselines[0].Count)
	}

	// Run the comparison.
	cmp, err := w.CompareDeployBaseline(context.Background(), beacondb.KindOutcome, "signup.completed", deployTime)
	if err != nil {
		t.Fatal(err)
	}
	// Baseline=96, Current=96 → ratio=1.0, verdict=pass.
	if cmp.Verdict != VerdictPass {
		t.Errorf("verdict = %s, want pass. Comparison: %+v", cmp.Verdict, cmp)
	}
	if cmp.Baseline != 96 {
		t.Errorf("baseline = %d, want 96", cmp.Baseline)
	}
	if cmp.Current != 96 {
		t.Errorf("current = %d, want 96", cmp.Current)
	}
}

func TestDeployToCompareFlow_failVerdict(t *testing.T) {
	// Same setup as the pass test, but the post-deploy traffic collapses to
	// half, so the verdict is drift or fail depending on the ratio.
	deployTime := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	now := deployTime.Add(24 * time.Hour)
	w, fake := newTestWorker(t, now)

	for h := 0; h < 24; h++ {
		for i := 0; i < 4; i++ {
			seedEvents(t, fake, beacondb.Event{
				Kind:      beacondb.KindOutcome,
				Name:      "signup.completed",
				CreatedAt: deployTime.Add(-time.Duration(24-h)*time.Hour + time.Duration(i)*time.Minute),
			})
		}
	}
	seedEvents(t, fake, beacondb.Event{Kind: beacondb.KindOutcome, Name: DeployEventName, CreatedAt: deployTime})
	// Post-deploy: drastically reduced traffic (~20% of baseline).
	for h := 0; h < 24; h++ {
		seedEvents(t, fake, beacondb.Event{
			Kind: beacondb.KindOutcome, Name: "signup.completed",
			CreatedAt: deployTime.Add(time.Duration(h) * time.Hour),
		})
	}

	if err := w.RecomputeRange(context.Background(), deployTime.Add(-24*time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	cmp, err := w.CompareDeployBaseline(context.Background(), beacondb.KindOutcome, "signup.completed", deployTime)
	if err != nil {
		t.Fatal(err)
	}
	// baseline=96, current=24 → ratio 0.25 → fail
	if cmp.Verdict != VerdictFail {
		t.Errorf("verdict = %s, want fail. Comparison: %+v", cmp.Verdict, cmp)
	}
}

func TestCompareDeployBaseline_insufficientWhenMissing(t *testing.T) {
	w, _ := newTestWorker(t, fixedNow)
	cmp, err := w.CompareDeployBaseline(context.Background(), beacondb.KindOutcome, "missing", fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Verdict != VerdictInsufficient {
		t.Errorf("verdict = %s, want insufficient", cmp.Verdict)
	}
}
