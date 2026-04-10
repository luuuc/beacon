// Package beacondb defines the database adapter contract every backend
// (PostgreSQL, MySQL, SQLite) must satisfy, plus the shared data types and
// cross-adapter helpers (canonical JSON, dimension hashing).
//
// No adapter implementations live here. Real adapters land in cards 3–5 and
// ship in their own subpackages (internal/beacondb/pgadapter, …). An
// in-memory fake in internal/beacondb/memfake backs the conformance suite.
package beacondb

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// Kind is the pillar an event or metric belongs to.
//
// beacon_events can only carry outcome, perf, or error. beacon_metrics adds
// the fourth value "baseline" for deployment + trailing baseline rows.
type Kind string

const (
	KindOutcome  Kind = "outcome"
	KindPerf     Kind = "perf"
	KindError    Kind = "error"
	KindBaseline Kind = "baseline" // metrics only
)

// Valid reports whether k is one of the known pillars.
// baseline is valid for metrics only; callers that reject it on events
// should do so explicitly.
func (k Kind) Valid() bool {
	switch k {
	case KindOutcome, KindPerf, KindError, KindBaseline:
		return true
	}
	return false
}

// PeriodKind is the shape of a beacon_metrics row.
//
// For a regular rollup, PeriodKind and Metric.PeriodWindow match
// ("hour"/"day"/"week"). For a baseline row, PeriodKind is "baseline" and
// PeriodWindow encodes the trailing window ("24h", "7d", "30d"). See
// doc/definition/03-data-model.md for the rationale.
type PeriodKind string

const (
	PeriodHour     PeriodKind = "hour"
	PeriodDay      PeriodKind = "day"
	PeriodWeek     PeriodKind = "week"
	PeriodBaseline PeriodKind = "baseline"
)

// Event is one raw row of beacon_events. Pointer fields are nullable in SQL.
type Event struct {
	ID          int64
	Kind        Kind
	Name        string
	ActorType   string
	ActorID     int64
	DurationMs  *int32
	Status      *int32
	Fingerprint string         // error events only; "" otherwise
	Properties  map[string]any // open schema
	Context     map[string]any // closed schema: request_id, deploy_sha, environment, host
	CreatedAt   time.Time
}

// Metric is one row of beacon_metrics. Percentile fields are nullable in SQL.
type Metric struct {
	ID            int64
	Kind          Kind
	Name          string
	PeriodKind    PeriodKind
	PeriodWindow  string // "hour"|"day"|"week"|"24h"|"7d"|"30d"
	PeriodStart   time.Time
	Count         int64
	Sum           *float64
	P50           *float64
	P95           *float64
	P99           *float64
	Fingerprint   string         // error rollups only
	Dimensions    map[string]any // nil == no dimensions
	DimensionHash string         // SHA256 hex of canonical JSON; "" for empty
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// EventFilter narrows ListEvents. Zero-value fields are unbounded, including
// Kind: a zero-value Kind returns every pillar (the rollup worker uses this
// to pull a whole time window in one query).
type EventFilter struct {
	Kind        Kind      // "" = any
	Name        string    // "" = any
	Fingerprint string    // "" = any
	Since       time.Time // zero = -∞
	Until       time.Time // zero = +∞
	Limit       int       // 0 = no limit
}

// MetricFilter narrows ListMetrics. Zero-value fields are unbounded.
type MetricFilter struct {
	Kind         Kind
	Name         string
	PeriodKind   PeriodKind // "" = any
	PeriodWindow string     // "" = any
	Fingerprint  string     // "" = any
	Since        time.Time
	Until        time.Time
	Limit        int
}

// Adapter is the full contract every backend implements. Implementations
// must be safe for concurrent use; Beacon's HTTP layer and rollup worker
// both call through the same adapter instance.
//
// Migration notes for implementers:
//   - Migrate must be idempotent; Beacon invokes it on every boot.
//   - Partial indexes (e.g. fingerprint IS NOT NULL) are native on PostgreSQL
//     and SQLite. MySQL emulates them with a generated column + full index;
//     the adapter hides the difference from callers.
//   - Upsert semantics on UpsertMetrics follow the unique key
//     (kind, name, period_kind, period_window, period_start, fingerprint,
//     dimension_hash). An existing row's counts/percentiles are replaced
//     (not summed) and updated_at is bumped.
type Adapter interface {
	// Ping verifies the database is reachable. Used by /readyz's db_reachable check.
	Ping(ctx context.Context) error

	// Migrate applies all pending schema migrations idempotently.
	Migrate(ctx context.Context) error

	// MigrationsApplied reports whether every known migration has run.
	// Used by /readyz's migrations_applied check.
	MigrationsApplied(ctx context.Context) (bool, error)

	// InsertEvents writes a batch of events atomically. The adapter assigns
	// auto-increment IDs and returns them in input order. An empty batch is
	// a no-op and returns a nil slice.
	InsertEvents(ctx context.Context, events []Event) ([]int64, error)

	// ListEvents returns events matching the filter ordered by created_at ASC, id ASC.
	ListEvents(ctx context.Context, filter EventFilter) ([]Event, error)

	// UpsertMetrics writes rollup rows. Rows matching an existing unique key
	// replace the row's count/sum/percentiles and bump updated_at; rows with
	// a new key are inserted. The batch as a whole is atomic.
	UpsertMetrics(ctx context.Context, metrics []Metric) error

	// ListMetrics returns metric rows matching the filter ordered by period_start ASC.
	ListMetrics(ctx context.Context, filter MetricFilter) ([]Metric, error)

	// DeleteEventsOlderThan removes raw events with created_at < cutoff.
	// Returns the number of rows deleted.
	DeleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int64, error)

	// Close releases the adapter's resources.
	Close() error
}

// ---------------------------------------------------------------------------
// Canonical JSON + dimension hashing (shared across every adapter).
// ---------------------------------------------------------------------------

// CanonicalJSON serializes v to a deterministic byte form:
//
//   - map keys are sorted lexicographically at every depth
//   - no insignificant whitespace
//   - values encode via Go's encoding/json defaults
//
// Slices preserve their input order (sequences are semantically ordered).
// Supported value types: nil, bool, string, all integer widths, float32/64,
// map[string]any, []any. Any other type is rejected.
//
// The result is not RFC 8785 compliant — it is the portable subset Beacon
// needs to compute identical dimension hashes across PostgreSQL, MySQL, and
// SQLite without relying on database JSON normalization.
func CanonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeCanonical(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeCanonical(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool, string,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64,
		json.Number:
		b, err := json.Marshal(x)
		if err != nil {
			return fmt.Errorf("canonical json: marshal scalar: %w", err)
		}
		buf.Write(b)
		return nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return fmt.Errorf("canonical json: marshal key %q: %w", k, err)
			}
			buf.Write(kb)
			buf.WriteByte(':')
			if err := encodeCanonical(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
		return nil
	case []any:
		buf.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := encodeCanonical(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
		return nil
	default:
		return fmt.Errorf("canonical json: unsupported type %T", v)
	}
}

// DimensionHash returns the dimension hash used in the beacon_metrics unique
// index. Empty or nil dimensions return the fixed sentinel "" (empty string)
// so rows without dimensions can be distinguished from dimensioned rows in
// the unique index without NULL handling.
func DimensionHash(dims map[string]any) (string, error) {
	if len(dims) == 0 {
		return "", nil
	}
	payload, err := CanonicalJSON(dims)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}
