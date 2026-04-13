package rollup

import (
	"context"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/memfake"
)

// A deterministic clock anchored to mid-hour so boundary logic gets exercised.
var fixedNow = time.Date(2026, 4, 10, 12, 30, 0, 0, time.UTC)

func newTestWorker(t *testing.T, now time.Time) (*Worker, *memfake.Fake) {
	t.Helper()
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	w := NewWorker(Config{TickInterval: time.Minute, RetentionRaw: 14 * 24 * time.Hour}, fake, nil)
	w.now = func() time.Time { return now }
	return w, fake
}

func dur(v int32) *int32 { return &v }

func seedEvents(t *testing.T, fake *memfake.Fake, events ...beacondb.Event) {
	t.Helper()
	if _, err := fake.InsertEvents(context.Background(), events); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Basic aggregation + per-kind shape
// ---------------------------------------------------------------------------

func TestAggregate_noEventsNoMetrics(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{})
	if len(rows) != 0 {
		t.Errorf("expected no metrics, got %d: %+v", len(rows), rows)
	}
	if w.LastTick().IsZero() {
		t.Error("LastTick should have advanced even with no events")
	}
}

func TestAggregate_outcomeCount(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup.completed", CreatedAt: hour.Add(1 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup.completed", CreatedAt: hour.Add(5 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup.completed", CreatedAt: hour.Add(10 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "checkout.failed", CreatedAt: hour.Add(11 * time.Minute)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindOutcome, PeriodKind: beacondb.PeriodHour})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (two distinct names)", len(rows))
	}
	for _, r := range rows {
		if r.PeriodKind != beacondb.PeriodHour || r.PeriodWindow != "hour" {
			t.Errorf("bad period: %+v", r)
		}
		if !r.PeriodStart.Equal(hour) {
			t.Errorf("period_start = %s, want %s", r.PeriodStart, hour)
		}
		if r.Name == "signup.completed" && r.Count != 3 {
			t.Errorf("signup count = %d, want 3", r.Count)
		}
		if r.Name == "checkout.failed" && r.Count != 1 {
			t.Errorf("checkout count = %d, want 1", r.Count)
		}
		if r.Sum != nil || r.P50 != nil {
			t.Errorf("outcome rollup should not carry duration stats: %+v", r)
		}
	}
}

func TestAggregate_perfPercentiles(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	// 10 latencies: 10, 20, ..., 100
	var evs []beacondb.Event
	for i := 1; i <= 10; i++ {
		evs = append(evs, beacondb.Event{
			Kind:       beacondb.KindPerf,
			Name:       "GET /",
			DurationMs: dur(int32(i * 10)),
			CreatedAt:  hour.Add(time.Duration(i) * time.Second),
		})
	}
	seedEvents(t, fake, evs...)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindPerf, Name: "GET /", PeriodKind: beacondb.PeriodHour,
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	r := rows[0]
	if r.Count != 10 {
		t.Errorf("count = %d", r.Count)
	}
	if r.Sum == nil || *r.Sum != 550 {
		t.Errorf("sum = %v, want 550", r.Sum)
	}
	// Nearest-rank for n=10:
	//   p50 → idx=ceil(0.5*10)-1 = 4 → 50
	//   p95 → idx=ceil(0.95*10)-1 = 9 → 100
	//   p99 → idx=ceil(0.99*10)-1 = 9 → 100
	if r.P50 == nil || *r.P50 != 50 {
		t.Errorf("p50 = %v, want 50", r.P50)
	}
	if r.P95 == nil || *r.P95 != 100 {
		t.Errorf("p95 = %v, want 100", r.P95)
	}
	if r.P99 == nil || *r.P99 != 100 {
		t.Errorf("p99 = %v, want 100", r.P99)
	}
}

func TestAggregate_errorKeyedByFingerprint(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-A", CreatedAt: hour.Add(time.Minute)},
		beacondb.Event{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-A", CreatedAt: hour.Add(2 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindError, Name: "NoMethodError", Fingerprint: "fp-B", CreatedAt: hour.Add(3 * time.Minute)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindError, Name: "NoMethodError", PeriodKind: beacondb.PeriodHour})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (one per fingerprint)", len(rows))
	}
	byFP := map[string]int64{}
	for _, r := range rows {
		byFP[r.Fingerprint] = r.Count
	}
	if byFP["fp-A"] != 2 || byFP["fp-B"] != 1 {
		t.Errorf("counts = %v, want {fp-A:2, fp-B:1}", byFP)
	}
}

// ---------------------------------------------------------------------------
// Per-dimension rollup
// ---------------------------------------------------------------------------

func TestAggregate_perDimensionRollup(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "http_request", Dimensions: map[string]any{"country": "US"}, CreatedAt: hour.Add(1 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "http_request", Dimensions: map[string]any{"country": "US"}, CreatedAt: hour.Add(2 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "http_request", Dimensions: map[string]any{"country": "DE"}, CreatedAt: hour.Add(3 * time.Minute)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindAmbient, Name: "http_request", PeriodKind: beacondb.PeriodHour,
	})

	// Expect 3 rows: undimensioned aggregate (count=3), US slice (count=2), DE slice (count=1).
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3 (aggregate + US + DE)", len(rows))
	}

	byHash := map[string]beacondb.Metric{}
	for _, r := range rows {
		byHash[r.DimensionHash] = r
	}

	// Undimensioned aggregate.
	agg, ok := byHash[""]
	if !ok {
		t.Fatal("missing undimensioned aggregate row")
	}
	if agg.Count != 3 {
		t.Errorf("aggregate count = %d, want 3", agg.Count)
	}

	// Per-country slices.
	usHash, _ := beacondb.DimensionHash(map[string]any{"country": "US"})
	deHash, _ := beacondb.DimensionHash(map[string]any{"country": "DE"})
	if us, ok := byHash[usHash]; !ok {
		t.Error("missing US dimension row")
	} else {
		if us.Count != 2 {
			t.Errorf("US count = %d, want 2", us.Count)
		}
		if us.Dimensions["country"] != "US" {
			t.Errorf("US dimensions = %+v, want {country: US}", us.Dimensions)
		}
	}
	if de, ok := byHash[deHash]; !ok {
		t.Error("missing DE dimension row")
	} else {
		if de.Count != 1 {
			t.Errorf("DE count = %d, want 1", de.Count)
		}
		if de.Dimensions["country"] != "DE" {
			t.Errorf("DE dimensions = %+v, want {country: DE}", de.Dimensions)
		}
	}
}

func TestAggregate_noDimensionsProducesUndimensionedOnly(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "job_lifecycle", CreatedAt: hour.Add(1 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "job_lifecycle", CreatedAt: hour.Add(2 * time.Minute)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindAmbient, Name: "job_lifecycle", PeriodKind: beacondb.PeriodHour,
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (undimensioned only)", len(rows))
	}
	if rows[0].Count != 2 {
		t.Errorf("count = %d, want 2", rows[0].Count)
	}
	if rows[0].DimensionHash != "" {
		t.Errorf("dimension_hash = %q, want empty", rows[0].DimensionHash)
	}
}

func TestAggregate_perfWithDimensions(t *testing.T) {
	// Enrichment applies to perf events too, not just ambient.
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(100), Dimensions: map[string]any{"country": "US"}, CreatedAt: hour.Add(1 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(200), Dimensions: map[string]any{"country": "DE"}, CreatedAt: hour.Add(2 * time.Minute)},
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(150), CreatedAt: hour.Add(3 * time.Minute)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindPerf, Name: "GET /", PeriodKind: beacondb.PeriodHour,
	})
	// 3 rows: undimensioned (count=3), US (count=1), DE (count=1).
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	byHash := map[string]beacondb.Metric{}
	for _, r := range rows {
		byHash[r.DimensionHash] = r
	}
	agg := byHash[""]
	if agg.Count != 3 {
		t.Errorf("aggregate count = %d, want 3", agg.Count)
	}
	if agg.P50 == nil || agg.Sum == nil {
		t.Error("aggregate should have duration stats")
	}
}

// ---------------------------------------------------------------------------
// Idempotence + late-arriving
// ---------------------------------------------------------------------------

func TestAggregate_idempotent(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(100), CreatedAt: hour.Add(time.Minute)},
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(200), CreatedAt: hour.Add(2 * time.Minute)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindPerf, PeriodKind: beacondb.PeriodHour})
	if len(before) != 1 {
		t.Fatalf("first run rows = %d", len(before))
	}

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindPerf, PeriodKind: beacondb.PeriodHour})
	if len(after) != 1 {
		t.Errorf("second run produced extra rows: %d", len(after))
	}
	if before[0].Count != after[0].Count || *before[0].P95 != *after[0].P95 {
		t.Errorf("non-idempotent: before=%+v after=%+v", before[0], after[0])
	}
	// The upsert should bump updated_at on the re-run.
	if !after[0].UpdatedAt.After(before[0].UpdatedAt) {
		t.Errorf("updated_at did not advance: %v → %v", before[0].UpdatedAt, after[0].UpdatedAt)
	}
}

func TestAggregate_lateArrivingEvent(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	hour := fixedNow.Truncate(time.Hour)

	// First pass: one event in the current hour.
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "x", CreatedAt: hour.Add(5 * time.Minute)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour})
	if len(rows) != 1 || rows[0].Count != 1 {
		t.Fatalf("initial rollup: %+v", rows)
	}

	// A late-arriving event with a created_at earlier in the same hour lands
	// after the first tick. The next tick must re-aggregate and bump count.
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "x", CreatedAt: hour.Add(2 * time.Minute)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ = fake.ListMetrics(context.Background(), beacondb.MetricFilter{Kind: beacondb.KindOutcome, Name: "x", PeriodKind: beacondb.PeriodHour})
	if len(rows) != 1 || rows[0].Count != 2 {
		t.Fatalf("after late event: %+v (want count=2)", rows)
	}
}

func TestAggregate_previousHourCoveredByEachTick(t *testing.T) {
	// Mid-hour tick at 12:30. A "boundary" event landing at 11:58 (previous
	// hour) must produce a rollup in the 11:00 bucket.
	w, fake := newTestWorker(t, fixedNow)
	prevHour := fixedNow.Truncate(time.Hour).Add(-time.Hour)
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "boundary", CreatedAt: prevHour.Add(58 * time.Minute)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, _ := fake.ListMetrics(context.Background(), beacondb.MetricFilter{
		Kind: beacondb.KindOutcome, Name: "boundary", PeriodKind: beacondb.PeriodHour,
	})
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	if !rows[0].PeriodStart.Equal(prevHour) {
		t.Errorf("period_start = %s, want %s", rows[0].PeriodStart, prevHour)
	}
}

// ---------------------------------------------------------------------------
// Prune
// ---------------------------------------------------------------------------

func TestPrune_runsAfterConfiguredTime(t *testing.T) {
	// Prune at 03:00 UTC. "Now" is 03:05 UTC, so prune should fire.
	now := time.Date(2026, 4, 10, 3, 5, 0, 0, time.UTC)
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{
		TickInterval: time.Minute,
		RetentionRaw: 14 * 24 * time.Hour,
		PruneAt:      "03:00",
		Timezone:     time.UTC,
	}, fake, nil)
	w.now = func() time.Time { return now }

	// Old (outside retention) and fresh events.
	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "ancient", CreatedAt: now.Add(-20 * 24 * time.Hour)},
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "recent", CreatedAt: now.Add(-1 * time.Hour)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{})
	if len(remaining) != 1 || remaining[0].Name != "recent" {
		t.Errorf("remaining = %+v", remaining)
	}
}

func TestPrune_skippedBeforeConfiguredTime(t *testing.T) {
	// 02:59 is before the 03:00 prune window — nothing should be deleted.
	now := time.Date(2026, 4, 10, 2, 59, 0, 0, time.UTC)
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{
		PruneAt:      "03:00",
		RetentionRaw: 14 * 24 * time.Hour,
		Timezone:     time.UTC,
	}, fake, nil)
	w.now = func() time.Time { return now }

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "ancient", CreatedAt: now.Add(-20 * 24 * time.Hour)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{})
	if len(remaining) != 1 {
		t.Errorf("prune ran too early: %d remaining", len(remaining))
	}
}

func TestPrune_runsOncePerDay(t *testing.T) {
	now := time.Date(2026, 4, 10, 3, 5, 0, 0, time.UTC)
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{
		PruneAt:      "03:00",
		RetentionRaw: 14 * 24 * time.Hour,
		Timezone:     time.UTC,
	}, fake, nil)
	w.now = func() time.Time { return now }

	seedEvents(t, fake,
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "ancient", CreatedAt: now.Add(-20 * 24 * time.Hour)},
	)
	// Two ticks within the same day. The second must not re-run prune (there
	// would be nothing to delete anyway, but shouldPrune() should also be
	// false so we exercise the "once per day" guard directly).
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.shouldPrune(now) {
		t.Error("shouldPrune should return false on the second call of the same day")
	}
}

func TestPrune_cutoffBoundary(t *testing.T) {
	now := time.Date(2026, 4, 10, 3, 5, 0, 0, time.UTC)
	retention := 14 * 24 * time.Hour
	cutoff := now.Add(-retention)

	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{PruneAt: "03:00", RetentionRaw: retention, Timezone: time.UTC}, fake, nil)
	w.now = func() time.Time { return now }

	seedEvents(t, fake,
		// Strictly before cutoff — should be deleted.
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "before", CreatedAt: cutoff.Add(-time.Second)},
		// Exactly at cutoff — DeleteEventsOlderThan uses strict <, so this stays.
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "at", CreatedAt: cutoff},
		// After cutoff — stays.
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "after", CreatedAt: cutoff.Add(time.Second)},
	)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	remaining, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{})
	names := map[string]bool{}
	for _, e := range remaining {
		names[e.Name] = true
	}
	if names["before"] {
		t.Error("event strictly before cutoff should have been deleted")
	}
	if !names["at"] || !names["after"] {
		t.Errorf("at-cutoff/after events should remain: %+v", names)
	}
}

// ---------------------------------------------------------------------------
// Two-tier retention (ambient vs standard)
// ---------------------------------------------------------------------------

func TestPrune_twoTierRetention(t *testing.T) {
	// Prune at 03:00 UTC. "Now" is 03:05 UTC, so prune fires.
	// Ambient retention: 24h. Standard retention: 14d.
	now := time.Date(2026, 4, 10, 3, 5, 0, 0, time.UTC)
	fake := memfake.New()
	if err := fake.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := NewWorker(Config{
		TickInterval:     time.Minute,
		RetentionRaw:     14 * 24 * time.Hour,
		AmbientRetention: 24 * time.Hour,
		PruneAt:          "03:00",
		Timezone:         time.UTC,
	}, fake, nil)
	w.now = func() time.Time { return now }

	seedEvents(t, fake,
		// Ambient event 2 days old — should be pruned (>24h).
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "http_request", CreatedAt: now.Add(-48 * time.Hour)},
		// Ambient event 12 hours old — should survive (<24h).
		beacondb.Event{Kind: beacondb.KindAmbient, Name: "http_request", CreatedAt: now.Add(-12 * time.Hour)},
		// Perf event 2 days old — should survive (within 14d).
		beacondb.Event{Kind: beacondb.KindPerf, Name: "GET /", DurationMs: dur(100), CreatedAt: now.Add(-48 * time.Hour)},
		// Outcome event 20 days old — should be pruned (>14d).
		beacondb.Event{Kind: beacondb.KindOutcome, Name: "signup", CreatedAt: now.Add(-20 * 24 * time.Hour)},
	)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	remaining, _ := fake.ListEvents(context.Background(), beacondb.EventFilter{})
	if len(remaining) != 2 {
		t.Fatalf("remaining = %d, want 2 (ambient@12h, perf@2d)", len(remaining))
	}

	byKind := map[beacondb.Kind]int{}
	for _, e := range remaining {
		byKind[e.Kind]++
	}
	if byKind[beacondb.KindAmbient] != 1 {
		t.Errorf("ambient remaining = %d, want 1", byKind[beacondb.KindAmbient])
	}
	if byKind[beacondb.KindPerf] != 1 {
		t.Errorf("perf remaining = %d, want 1", byKind[beacondb.KindPerf])
	}
}

// ---------------------------------------------------------------------------
// LastTick advancement
// ---------------------------------------------------------------------------

func TestLastTickAdvancesOnlyOnSuccess(t *testing.T) {
	w, fake := newTestWorker(t, fixedNow)
	if !w.LastTick().IsZero() {
		t.Error("LastTick should start at zero")
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !w.LastTick().Equal(fixedNow.UTC()) {
		t.Errorf("LastTick = %s, want %s", w.LastTick(), fixedNow.UTC())
	}

	// Close the adapter to force aggregate to fail on the next tick.
	_ = fake.Close()
	before := w.LastTick()
	advance := fixedNow.Add(time.Minute)
	w.now = func() time.Time { return advance }
	err := w.RunOnce(context.Background())
	if err == nil {
		t.Error("expected error after close")
	}
	if !w.LastTick().Equal(before) {
		t.Errorf("LastTick advanced on failure: %s → %s", before, w.LastTick())
	}
}

// ---------------------------------------------------------------------------
// Percentile helper
// ---------------------------------------------------------------------------

func TestPercentileNearestRank(t *testing.T) {
	cases := []struct {
		name string
		data []float64
		p    float64
		want float64
	}{
		{"single", []float64{42}, 0.5, 42},
		{"p50 odd", []float64{10, 20, 30, 40, 50}, 0.5, 30},
		{"p95 of 100", linspace(1, 100), 0.95, 95},
		{"p99 of 100", linspace(1, 100), 0.99, 99},
		{"p100 clamps", []float64{1, 2, 3}, 1.0, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentile(tc.data, tc.p)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func linspace(from, to int) []float64 {
	out := make([]float64, 0, to-from+1)
	for i := from; i <= to; i++ {
		out = append(out, float64(i))
	}
	return out
}

func TestParseHHMM(t *testing.T) {
	cases := []struct {
		in      string
		hh, mm  int
		wantErr bool
	}{
		{"03:00", 3, 0, false},
		{"23:59", 23, 59, false},
		{"00:00", 0, 0, false},
		{"3:00", 3, 0, false},
		{"24:00", 0, 0, true},
		{"03:60", 0, 0, true},
		{"bogus", 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			hh, mm, err := parseHHMM(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Error("expected error")
				}
				return
			}
			if err != nil || hh != tc.hh || mm != tc.mm {
				t.Errorf("got (%d, %d, %v), want (%d, %d, nil)", hh, mm, err, tc.hh, tc.mm)
			}
		})
	}
}
