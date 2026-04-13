package beacondb

import (
	"context"
	"testing"
	"time"
)

// RunConformance runs the full adapter behavioral test suite against any
// implementation of Adapter. Real adapters (PostgreSQL, MySQL, SQLite) and
// the in-memory fake all use this suite — it is the contract.
//
// The factory is called once per subtest and must return a fresh, empty
// adapter. RunConformance closes the adapter at the end of each subtest.
func RunConformance(t *testing.T, factory func(tb testing.TB) Adapter) {
	t.Helper()

	// Raw subtests run against an un-migrated adapter.
	runRaw := func(name string, fn func(t *testing.T, a Adapter)) {
		t.Run(name, func(t *testing.T) {
			a := factory(t)
			t.Cleanup(func() { _ = a.Close() })
			fn(t, a)
		})
	}
	// Migrated subtests receive an adapter with Migrate already applied.
	run := func(name string, fn func(t *testing.T, a Adapter)) {
		t.Run(name, func(t *testing.T) {
			a := factory(t)
			t.Cleanup(func() { _ = a.Close() })
			if err := a.Migrate(context.Background()); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
			fn(t, a)
		})
	}

	runRaw("Migrate_idempotent", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		if err := a.Migrate(ctx); err != nil {
			t.Fatalf("first Migrate: %v", err)
		}
		if err := a.Migrate(ctx); err != nil {
			t.Fatalf("second Migrate: %v", err)
		}
		applied, err := a.MigrationsApplied(ctx)
		if err != nil {
			t.Fatalf("MigrationsApplied: %v", err)
		}
		if !applied {
			t.Error("MigrationsApplied returned false after Migrate")
		}
	})

	runRaw("MigrationsApplied_falseBeforeMigrate", func(t *testing.T, a Adapter) {
		applied, err := a.MigrationsApplied(context.Background())
		if err != nil {
			t.Fatalf("MigrationsApplied: %v", err)
		}
		if applied {
			t.Error("MigrationsApplied returned true on a fresh adapter")
		}
	})

	run("Ping", func(t *testing.T, a Adapter) {
		if err := a.Ping(context.Background()); err != nil {
			t.Errorf("Ping: %v", err)
		}
	})

	run("InsertEvents_assignsIDsInOrder", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		events := []Event{
			{Kind: KindOutcome, Name: "signup.completed", CreatedAt: t0(0)},
			{Kind: KindPerf, Name: "GET /dashboard", CreatedAt: t0(1)},
			{Kind: KindError, Name: "NoMethodError", Fingerprint: "abc", CreatedAt: t0(2)},
		}
		ids, err := a.InsertEvents(ctx, events)
		if err != nil {
			t.Fatalf("InsertEvents: %v", err)
		}
		if len(ids) != 3 {
			t.Fatalf("ids = %d, want 3", len(ids))
		}
		for i := 1; i < len(ids); i++ {
			if ids[i] <= ids[i-1] {
				t.Errorf("ids not monotonic: %v", ids)
			}
		}
	})

	run("InsertEvents_emptyBatchIsNoOp", func(t *testing.T, a Adapter) {
		ids, err := a.InsertEvents(context.Background(), nil)
		if err != nil {
			t.Fatalf("InsertEvents nil: %v", err)
		}
		if len(ids) != 0 {
			t.Errorf("expected empty ids, got %v", ids)
		}
	})

	run("ListEvents_filtersAndOrdering", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		seed := []Event{
			{Kind: KindOutcome, Name: "signup.completed", CreatedAt: t0(0)},
			{Kind: KindOutcome, Name: "checkout.failed", CreatedAt: t0(1)},
			{Kind: KindOutcome, Name: "signup.completed", CreatedAt: t0(2)},
			{Kind: KindPerf, Name: "GET /", CreatedAt: t0(3)},
			{Kind: KindError, Name: "NoMethodError", Fingerprint: "abc", CreatedAt: t0(4)},
		}
		if _, err := a.InsertEvents(ctx, seed); err != nil {
			t.Fatalf("InsertEvents: %v", err)
		}

		all, err := a.ListEvents(ctx, EventFilter{Kind: KindOutcome})
		if err != nil {
			t.Fatalf("ListEvents: %v", err)
		}
		if len(all) != 3 {
			t.Errorf("outcome rows = %d, want 3", len(all))
		}
		// Ordering: ascending by CreatedAt.
		for i := 1; i < len(all); i++ {
			if all[i].CreatedAt.Before(all[i-1].CreatedAt) {
				t.Errorf("not sorted: %v vs %v", all[i-1].CreatedAt, all[i].CreatedAt)
			}
		}

		named, err := a.ListEvents(ctx, EventFilter{Kind: KindOutcome, Name: "signup.completed"})
		if err != nil {
			t.Fatalf("ListEvents by name: %v", err)
		}
		if len(named) != 2 {
			t.Errorf("signup rows = %d, want 2", len(named))
		}

		window, err := a.ListEvents(ctx, EventFilter{
			Kind:  KindOutcome,
			Since: t0(1),
			Until: t0(3),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(window) != 2 {
			t.Errorf("time-window rows = %d, want 2", len(window))
		}

		limited, err := a.ListEvents(ctx, EventFilter{Kind: KindOutcome, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(limited) != 1 {
			t.Errorf("limit rows = %d, want 1", len(limited))
		}

		fp, err := a.ListEvents(ctx, EventFilter{Kind: KindError, Fingerprint: "abc"})
		if err != nil {
			t.Fatal(err)
		}
		if len(fp) != 1 {
			t.Errorf("fingerprint rows = %d, want 1", len(fp))
		}
	})

	run("UpsertMetrics_insertThenUpdateSameKey", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		hour := t0(0).Truncate(time.Hour)
		m := Metric{
			Kind: KindOutcome, Name: "signup.completed",
			PeriodKind: PeriodHour, PeriodWindow: "hour", PeriodStart: hour,
			Count: 10, DimensionHash: "",
		}
		if err := a.UpsertMetrics(ctx, []Metric{m}); err != nil {
			t.Fatalf("first upsert: %v", err)
		}
		rows, err := a.ListMetrics(ctx, MetricFilter{Kind: KindOutcome, Name: "signup.completed"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 || rows[0].Count != 10 {
			t.Fatalf("after insert: %+v", rows)
		}
		firstUpdated := rows[0].UpdatedAt

		// Update same key; count should be replaced, not summed.
		time.Sleep(2 * time.Millisecond)
		m.Count = 42
		if err := a.UpsertMetrics(ctx, []Metric{m}); err != nil {
			t.Fatalf("second upsert: %v", err)
		}
		rows, err = a.ListMetrics(ctx, MetricFilter{Kind: KindOutcome, Name: "signup.completed"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("after update: row count = %d, want 1", len(rows))
		}
		if rows[0].Count != 42 {
			t.Errorf("count = %d, want 42 (replace not sum)", rows[0].Count)
		}
		if !rows[0].UpdatedAt.After(firstUpdated) {
			t.Errorf("updated_at did not advance: %v → %v", firstUpdated, rows[0].UpdatedAt)
		}
	})

	run("UpsertMetrics_distinctDimensionHashAreDistinctRows", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		hour := t0(0).Truncate(time.Hour)
		base := Metric{
			Kind: KindOutcome, Name: "signup.completed",
			PeriodKind: PeriodHour, PeriodWindow: "hour", PeriodStart: hour,
		}
		pro := base
		pro.Dimensions = map[string]any{"plan": "pro"}
		pro.DimensionHash = mustHash(t, pro.Dimensions)
		pro.Count = 5

		free := base
		free.Dimensions = map[string]any{"plan": "free"}
		free.DimensionHash = mustHash(t, free.Dimensions)
		free.Count = 8

		agg := base
		agg.Count = 13 // empty dimensions row, separate from the two sliced rows

		if err := a.UpsertMetrics(ctx, []Metric{pro, free, agg}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		rows, err := a.ListMetrics(ctx, MetricFilter{Kind: KindOutcome, Name: "signup.completed"})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 3 {
			t.Errorf("rows = %d, want 3 (pro/free/aggregate)", len(rows))
		}
	})

	run("ListMetrics_periodAndTimeFilters", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		seed := []Metric{
			{Kind: KindPerf, Name: "GET /", PeriodKind: PeriodHour, PeriodWindow: "hour", PeriodStart: t0(0).Truncate(time.Hour), Count: 1},
			{Kind: KindPerf, Name: "GET /", PeriodKind: PeriodHour, PeriodWindow: "hour", PeriodStart: t0(0).Truncate(time.Hour).Add(time.Hour), Count: 2},
			{Kind: KindPerf, Name: "GET /", PeriodKind: PeriodDay, PeriodWindow: "day", PeriodStart: t0(0).Truncate(24 * time.Hour), Count: 3},
			{Kind: KindBaseline, Name: "GET /", PeriodKind: PeriodBaseline, PeriodWindow: "30d", PeriodStart: t0(0), Count: 99},
		}
		if err := a.UpsertMetrics(ctx, seed); err != nil {
			t.Fatal(err)
		}
		hourly, err := a.ListMetrics(ctx, MetricFilter{Kind: KindPerf, Name: "GET /", PeriodKind: PeriodHour})
		if err != nil {
			t.Fatal(err)
		}
		if len(hourly) != 2 {
			t.Errorf("hourly rows = %d, want 2", len(hourly))
		}
		baseline, err := a.ListMetrics(ctx, MetricFilter{Kind: KindBaseline, PeriodWindow: "30d"})
		if err != nil {
			t.Fatal(err)
		}
		if len(baseline) != 1 || baseline[0].Count != 99 {
			t.Errorf("baseline = %+v", baseline)
		}
	})

	run("DeleteEventsByKindOlderThan", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		seed := []Event{
			{Kind: KindAmbient, Name: "http_request", CreatedAt: t0(0)},
			{Kind: KindAmbient, Name: "http_request", CreatedAt: t0(10)},
			{Kind: KindPerf, Name: "GET /", CreatedAt: t0(0)},
			{Kind: KindOutcome, Name: "signup", CreatedAt: t0(0)},
		}
		if _, err := a.InsertEvents(ctx, seed); err != nil {
			t.Fatal(err)
		}
		// Delete only ambient events older than t0(5).
		n, err := a.DeleteEventsByKindOlderThan(ctx, KindAmbient, t0(5))
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("deleted = %d, want 1", n)
		}
		remaining, err := a.ListEvents(ctx, EventFilter{})
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 3 {
			t.Errorf("remaining = %d, want 3 (ambient@t10, perf@t0, outcome@t0)", len(remaining))
		}
		// Verify the perf and outcome events at t0 survived.
		for _, e := range remaining {
			if e.Kind == KindAmbient && e.CreatedAt.Equal(t0(0)) {
				t.Error("ambient event at t0 should have been deleted")
			}
		}
	})

	run("DeleteEventsOlderThan", func(t *testing.T, a Adapter) {
		ctx := context.Background()
		seed := []Event{
			{Kind: KindOutcome, Name: "old1", CreatedAt: t0(0)},
			{Kind: KindOutcome, Name: "old2", CreatedAt: t0(1)},
			{Kind: KindOutcome, Name: "new1", CreatedAt: t0(10)},
		}
		if _, err := a.InsertEvents(ctx, seed); err != nil {
			t.Fatal(err)
		}
		n, err := a.DeleteEventsOlderThan(ctx, t0(5))
		if err != nil {
			t.Fatal(err)
		}
		if n != 2 {
			t.Errorf("deleted = %d, want 2", n)
		}
		remaining, err := a.ListEvents(ctx, EventFilter{Kind: KindOutcome})
		if err != nil {
			t.Fatal(err)
		}
		if len(remaining) != 1 || remaining[0].Name != "new1" {
			t.Errorf("remaining = %+v", remaining)
		}
	})
}

// t0 is a deterministic clock helper for conformance tests.
// Returns a fixed base time offset by n seconds.
func t0(n int) time.Time {
	base := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	return base.Add(time.Duration(n) * time.Second)
}

func mustHash(t *testing.T, dims map[string]any) string {
	t.Helper()
	h, err := DimensionHash(dims)
	if err != nil {
		t.Fatalf("DimensionHash: %v", err)
	}
	return h
}
