// Package memfake is an in-memory Adapter implementation used by the
// conformance suite and by tests elsewhere in the binary that need an
// adapter without a real database. It is not a production adapter.
package memfake

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

type Fake struct {
	mu       sync.Mutex
	migrated bool
	closed   bool
	nextID   int64
	events   []beacondb.Event
	metrics  []beacondb.Metric
}

func New() *Fake { return &Fake{} }

func (f *Fake) Ping(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("memfake: closed")
	}
	return nil
}

func (f *Fake) Migrate(_ context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return errors.New("memfake: closed")
	}
	f.migrated = true
	return nil
}

func (f *Fake) MigrationsApplied(_ context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.migrated, nil
}

func (f *Fake) InsertEvents(_ context.Context, events []beacondb.Event) ([]int64, error) {
	if len(events) == 0 {
		return nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.requireMigrated(); err != nil {
		return nil, err
	}
	ids := make([]int64, len(events))
	for i := range events {
		f.nextID++
		e := events[i]
		e.ID = f.nextID
		if e.CreatedAt.IsZero() {
			e.CreatedAt = time.Now()
		}
		f.events = append(f.events, e)
		ids[i] = f.nextID
	}
	return ids, nil
}

func (f *Fake) ListEvents(_ context.Context, filter beacondb.EventFilter) ([]beacondb.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.requireMigrated(); err != nil {
		return nil, err
	}
	var out []beacondb.Event
	for _, e := range f.events {
		if filter.Kind != "" && e.Kind != filter.Kind {
			continue
		}
		if filter.Name != "" && e.Name != filter.Name {
			continue
		}
		if filter.Fingerprint != "" && e.Fingerprint != filter.Fingerprint {
			continue
		}
		if !filter.Since.IsZero() && e.CreatedAt.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && !e.CreatedAt.Before(filter.Until) {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

type metricKey struct {
	Kind          beacondb.Kind
	Name          string
	PeriodKind    beacondb.PeriodKind
	PeriodWindow  string
	PeriodStartNS int64
	Fingerprint   string
	DimensionHash string
}

func keyOf(m beacondb.Metric) metricKey {
	return metricKey{
		Kind:          m.Kind,
		Name:          m.Name,
		PeriodKind:    m.PeriodKind,
		PeriodWindow:  m.PeriodWindow,
		PeriodStartNS: m.PeriodStart.UnixNano(),
		Fingerprint:   m.Fingerprint,
		DimensionHash: m.DimensionHash,
	}
}

func (f *Fake) UpsertMetrics(_ context.Context, metrics []beacondb.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.requireMigrated(); err != nil {
		return err
	}
	now := time.Now()
	for _, m := range metrics {
		k := keyOf(m)
		matched := -1
		for i := range f.metrics {
			if keyOf(f.metrics[i]) == k {
				matched = i
				break
			}
		}
		if matched >= 0 {
			existing := f.metrics[matched]
			m.ID = existing.ID
			m.CreatedAt = existing.CreatedAt
			m.UpdatedAt = now
			f.metrics[matched] = m
			continue
		}
		f.nextID++
		m.ID = f.nextID
		if m.CreatedAt.IsZero() {
			m.CreatedAt = now
		}
		m.UpdatedAt = now
		f.metrics = append(f.metrics, m)
	}
	return nil
}

func (f *Fake) ListMetrics(_ context.Context, filter beacondb.MetricFilter) ([]beacondb.Metric, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.requireMigrated(); err != nil {
		return nil, err
	}
	var out []beacondb.Metric
	for _, m := range f.metrics {
		if filter.Kind != "" && m.Kind != filter.Kind {
			continue
		}
		if filter.Name != "" && m.Name != filter.Name {
			continue
		}
		if filter.PeriodKind != "" && m.PeriodKind != filter.PeriodKind {
			continue
		}
		if filter.PeriodWindow != "" && m.PeriodWindow != filter.PeriodWindow {
			continue
		}
		if filter.Fingerprint != "" && m.Fingerprint != filter.Fingerprint {
			continue
		}
		if !filter.Since.IsZero() && m.PeriodStart.Before(filter.Since) {
			continue
		}
		if !filter.Until.IsZero() && !m.PeriodStart.Before(filter.Until) {
			continue
		}
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].PeriodStart.Equal(out[j].PeriodStart) {
			return out[i].ID < out[j].ID
		}
		return out[i].PeriodStart.Before(out[j].PeriodStart)
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

func (f *Fake) DeleteEventsOlderThan(_ context.Context, cutoff time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.requireMigrated(); err != nil {
		return 0, err
	}
	kept := f.events[:0]
	var deleted int64
	for _, e := range f.events {
		if e.CreatedAt.Before(cutoff) {
			deleted++
			continue
		}
		kept = append(kept, e)
	}
	// Zero the tail to drop references held by the previous backing array.
	for i := len(kept); i < len(f.events); i++ {
		f.events[i] = beacondb.Event{}
	}
	f.events = kept
	return deleted, nil
}

func (f *Fake) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *Fake) requireMigrated() error {
	if f.closed {
		return errors.New("memfake: closed")
	}
	if !f.migrated {
		return errors.New("memfake: Migrate has not been called")
	}
	return nil
}
