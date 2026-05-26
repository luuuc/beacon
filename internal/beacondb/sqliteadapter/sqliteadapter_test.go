package sqliteadapter

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

func openTestAdapter(t *testing.T) *Adapter {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "beacon.db")
	a, err := Open(context.Background(), Config{Path: path})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()
	if err := a.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func TestSqliteadapterConformance(t *testing.T) {
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		dir := tb.TempDir()
		path := filepath.Join(dir, "beacon.db")
		a, err := Open(context.Background(), Config{Path: path})
		if err != nil {
			tb.Fatalf("Open: %v", err)
		}
		return a
	})
}

func TestDismissAnomaly(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := a.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindAmbient, Name: "test",
		PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h",
		PeriodStart: now, Count: 10, Fingerprint: "volume_shift",
	}}); err != nil {
		t.Fatal(err)
	}

	rows, _ := a.ListMetrics(ctx, beacondb.MetricFilter{PeriodKind: beacondb.PeriodAnomaly})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}

	if err := a.DismissAnomaly(ctx, rows[0].ID); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	// Already dismissed — should get ErrNotFound.
	err := a.DismissAnomaly(ctx, rows[0].ID)
	if err == nil {
		t.Fatal("expected error for re-dismiss")
	}

	// Non-existent ID.
	err = a.DismissAnomaly(ctx, 99999)
	if err == nil {
		t.Fatal("expected error for non-existent ID")
	}
}

func TestDeleteEventsByKindOlderThan(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()

	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	recent := now.Add(-1 * time.Hour)

	_, err := a.InsertEvents(ctx, []beacondb.Event{
		{Kind: beacondb.KindError, Name: "RuntimeError", CreatedAt: old},
		{Kind: beacondb.KindOutcome, Name: "signup", CreatedAt: old},
		{Kind: beacondb.KindError, Name: "RuntimeError", CreatedAt: recent},
	})
	if err != nil {
		t.Fatal(err)
	}

	deleted, err := a.DeleteEventsByKindOlderThan(ctx, beacondb.KindError, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}

	remaining, _ := a.ListEvents(ctx, beacondb.EventFilter{})
	if len(remaining) != 2 {
		t.Errorf("remaining = %d, want 2", len(remaining))
	}
}

func TestListMetrics_filterBranches(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour)
	if err := a.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindOutcome, Name: "signup", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: now, Count: 10, Fingerprint: "fp1"},
		{Kind: beacondb.KindPerf, Name: "GET /items", PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour", PeriodStart: now, Count: 20},
		{Kind: beacondb.KindOutcome, Name: "signup", PeriodKind: beacondb.PeriodBaseline, PeriodWindow: "24h", PeriodStart: now, Count: 100},
	}); err != nil {
		t.Fatal(err)
	}

	// Filter by name.
	rows, _ := a.ListMetrics(ctx, beacondb.MetricFilter{Name: "signup"})
	if len(rows) != 2 {
		t.Errorf("by name: %d, want 2", len(rows))
	}

	// Filter by fingerprint.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{Fingerprint: "fp1"})
	if len(rows) != 1 {
		t.Errorf("by fingerprint: %d, want 1", len(rows))
	}

	// Filter by period_window.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{PeriodWindow: "24h"})
	if len(rows) != 1 {
		t.Errorf("by period_window: %d, want 1", len(rows))
	}

	// Limit.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{Limit: 1})
	if len(rows) != 1 {
		t.Errorf("limit: %d, want 1", len(rows))
	}

	// Since/Until.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{Since: now.Add(-time.Hour), Until: now.Add(time.Hour)})
	if len(rows) != 3 {
		t.Errorf("since/until: %d, want 3", len(rows))
	}
}

func TestListMetrics_excludeDismissed(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := a.UpsertMetrics(ctx, []beacondb.Metric{
		{Kind: beacondb.KindAmbient, Name: "a", PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h", PeriodStart: now, Count: 5, Fingerprint: "volume_shift"},
		{Kind: beacondb.KindAmbient, Name: "b", PeriodKind: beacondb.PeriodAnomaly, PeriodWindow: "24h", PeriodStart: now, Count: 10, Fingerprint: "volume_shift"},
	}); err != nil {
		t.Fatal(err)
	}

	// Dismiss one.
	rows, _ := a.ListMetrics(ctx, beacondb.MetricFilter{PeriodKind: beacondb.PeriodAnomaly})
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if err := a.DismissAnomaly(ctx, rows[0].ID); err != nil {
		t.Fatal(err)
	}

	// Without exclude: 2 rows.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{PeriodKind: beacondb.PeriodAnomaly})
	if len(rows) != 2 {
		t.Errorf("without exclude: %d, want 2", len(rows))
	}

	// With exclude: 1 row.
	rows, _ = a.ListMetrics(ctx, beacondb.MetricFilter{PeriodKind: beacondb.PeriodAnomaly, ExcludeDismissed: true})
	if len(rows) != 1 {
		t.Errorf("with exclude: %d, want 1", len(rows))
	}
}

func TestUpsertMetrics_withDimensions(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Hour)
	dims := map[string]any{"plan": "pro", "country": "FR"}
	dh, _ := beacondb.DimensionHash(dims)

	if err := a.UpsertMetrics(ctx, []beacondb.Metric{{
		Kind: beacondb.KindOutcome, Name: "signup",
		PeriodKind: beacondb.PeriodHour, PeriodWindow: "hour",
		PeriodStart: now, Count: 5, Dimensions: dims, DimensionHash: dh,
	}}); err != nil {
		t.Fatal(err)
	}

	rows, _ := a.ListMetrics(ctx, beacondb.MetricFilter{Name: "signup"})
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].DimensionHash != dh {
		t.Errorf("dimension_hash = %q, want %q", rows[0].DimensionHash, dh)
	}
	if rows[0].Dimensions["plan"] != "pro" {
		t.Errorf("dimensions = %v", rows[0].Dimensions)
	}
}

func TestMigrationsApplied(t *testing.T) {
	a := openTestAdapter(t)
	ctx := context.Background()
	applied, err := a.MigrationsApplied(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Error("expected MigrationsApplied = true after Migrate()")
	}
}

func TestPing(t *testing.T) {
	a := openTestAdapter(t)
	if err := a.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}
