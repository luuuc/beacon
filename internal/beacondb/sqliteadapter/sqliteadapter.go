// Package sqliteadapter is the SQLite implementation of beacondb.Adapter.
// It uses modernc.org/sqlite (pure Go, no cgo) so beacon stays a single
// static binary.
//
// Storage model deltas from the Postgres adapter:
//
//   - Timestamps are INTEGER nanoseconds (UTC). Storing as TEXT would invite
//     format drift between clients; nanoseconds are portable and exact.
//   - JSON columns are TEXT. We validate + canonicalize in Go, never in SQL.
//   - Concurrency: SQLite serializes writes. We set MaxOpenConns=1 and enable
//     WAL so reads can happen alongside the single writer.
package sqliteadapter

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/luuuc/beacon/internal/beacondb"
)

type Config struct {
	// Path is the SQLite file path. ":memory:" is accepted for tests.
	Path string
}

type Adapter struct {
	db *sql.DB
}

func Open(ctx context.Context, cfg Config) (*Adapter, error) {
	if cfg.Path == "" {
		return nil, errors.New("sqliteadapter: Path is required")
	}
	// Use the URI form so we can set pragmas up front.
	// _pragma=foreign_keys(1) + busy_timeout + journal_mode=WAL.
	dsn := "file:" + cfg.Path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqliteadapter: open: %w", err)
	}
	// Serialize writes; SQLite doesn't benefit from a larger pool here.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqliteadapter: ping: %w", err)
	}
	return &Adapter{db: db}, nil
}

func (a *Adapter) Ping(ctx context.Context) error { return a.db.PingContext(ctx) }

func (a *Adapter) Migrate(ctx context.Context) error { return applyMigrations(ctx, a.db) }

func (a *Adapter) MigrationsApplied(ctx context.Context) (bool, error) {
	return migrationsApplied(ctx, a.db)
}

func (a *Adapter) Close() error { return a.db.Close() }

// ---------------------------------------------------------------------------
// beacon_events writes + reads
// ---------------------------------------------------------------------------

const insertEventSQL = `
INSERT INTO beacon_events
  (kind, name, actor_type, actor_id, duration_ms, status, fingerprint, properties, context, dimensions, created_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`

func (a *Adapter) InsertEvents(ctx context.Context, events []beacondb.Event) ([]int64, error) {
	if len(events) == 0 {
		return nil, nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, insertEventSQL)
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	ids := make([]int64, len(events))
	for i, e := range events {
		props, err := marshalJSON(e.Properties)
		if err != nil {
			return nil, fmt.Errorf("event[%d] properties: %w", i, err)
		}
		cx, err := marshalJSON(e.Context)
		if err != nil {
			return nil, fmt.Errorf("event[%d] context: %w", i, err)
		}
		dims, err := marshalJSON(e.Dimensions)
		if err != nil {
			return nil, fmt.Errorf("event[%d] dimensions: %w", i, err)
		}
		ts := e.CreatedAt
		if ts.IsZero() {
			ts = time.Now().UTC()
		}
		res, err := stmt.ExecContext(ctx,
			string(e.Kind), e.Name, e.ActorType, e.ActorID,
			nullableInt32(e.DurationMs), nullableInt32(e.Status),
			e.Fingerprint, string(props), string(cx), string(dims), ts.UnixNano(),
		)
		if err != nil {
			return nil, fmt.Errorf("insert event[%d]: %w", i, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return nil, err
		}
		ids[i] = id
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Adapter) ListEvents(ctx context.Context, filter beacondb.EventFilter) ([]beacondb.Event, error) {
	var (
		where []string
		args  []any
	)
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, string(filter.Kind))
	}
	if filter.Name != "" {
		where = append(where, "name = ?")
		args = append(args, filter.Name)
	}
	if filter.Fingerprint != "" {
		where = append(where, "fingerprint = ?")
		args = append(args, filter.Fingerprint)
	}
	if !filter.Since.IsZero() {
		where = append(where, "created_at >= ?")
		args = append(args, filter.Since.UnixNano())
	}
	if !filter.Until.IsZero() {
		where = append(where, "created_at < ?")
		args = append(args, filter.Until.UnixNano())
	}

	sqlStr := `
SELECT id, kind, name, actor_type, actor_id, duration_ms, status,
       fingerprint, properties, context, dimensions, created_at
  FROM beacon_events`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY created_at ASC, id ASC"
	if filter.Limit > 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := a.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []beacondb.Event
	for rows.Next() {
		var (
			e         beacondb.Event
			kind      string
			dur, stat sql.NullInt32
			props, cx, dims string
			createdNS int64
		)
		if err := rows.Scan(
			&e.ID, &kind, &e.Name, &e.ActorType, &e.ActorID,
			&dur, &stat, &e.Fingerprint, &props, &cx, &dims, &createdNS,
		); err != nil {
			return nil, err
		}
		e.Kind = beacondb.Kind(kind)
		if dur.Valid {
			v := dur.Int32
			e.DurationMs = &v
		}
		if stat.Valid {
			v := stat.Int32
			e.Status = &v
		}
		if err := unmarshalJSON([]byte(props), &e.Properties); err != nil {
			return nil, err
		}
		if err := unmarshalJSON([]byte(cx), &e.Context); err != nil {
			return nil, err
		}
		if err := unmarshalJSON([]byte(dims), &e.Dimensions); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(0, createdNS).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *Adapter) DismissAnomaly(ctx context.Context, id int64) error {
	now := time.Now().UnixNano()
	res, err := a.db.ExecContext(ctx,
		`UPDATE beacon_metrics SET dismissed_at = ?, updated_at = ?
		  WHERE id = ? AND period_kind = 'anomaly' AND dismissed_at IS NULL`, now, now, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("anomaly %d: %w", id, beacondb.ErrNotFound)
	}
	return nil
}

func (a *Adapter) DeleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	res, err := a.db.ExecContext(ctx, `DELETE FROM beacon_events WHERE created_at < ?`, cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (a *Adapter) DeleteEventsByKindOlderThan(ctx context.Context, kind beacondb.Kind, cutoff time.Time) (int64, error) {
	res, err := a.db.ExecContext(ctx, `DELETE FROM beacon_events WHERE kind = ? AND created_at < ?`, string(kind), cutoff.UnixNano())
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---------------------------------------------------------------------------
// beacon_metrics writes + reads
// ---------------------------------------------------------------------------

const upsertMetricSQL = `
INSERT INTO beacon_metrics
  (kind, name, period_kind, period_window, period_start, count, sum, p50, p95, p99,
   fingerprint, dimensions, dimension_hash, introduced_deploy_sha, created_at, updated_at)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT (kind, name, period_kind, period_window, period_start, fingerprint, dimension_hash)
DO UPDATE SET
  count      = excluded.count,
  sum        = excluded.sum,
  p50        = excluded.p50,
  p95        = excluded.p95,
  p99        = excluded.p99,
  dimensions = excluded.dimensions,
  introduced_deploy_sha = COALESCE(beacon_metrics.introduced_deploy_sha, excluded.introduced_deploy_sha),
  updated_at = excluded.updated_at`

func (a *Adapter) UpsertMetrics(ctx context.Context, metrics []beacondb.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertMetricSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for i, m := range metrics {
		dims, err := marshalJSON(m.Dimensions)
		if err != nil {
			return fmt.Errorf("metric[%d] dimensions: %w", i, err)
		}
		now := nowNS()
		if _, err := stmt.ExecContext(ctx,
			string(m.Kind), m.Name, string(m.PeriodKind), m.PeriodWindow, m.PeriodStart.UnixNano(),
			m.Count, m.Sum, m.P50, m.P95, m.P99,
			m.Fingerprint, string(dims), m.DimensionHash, nilIfEmpty(m.IntroducedDeploySHA),
			now, now,
		); err != nil {
			return fmt.Errorf("upsert metric[%d]: %w", i, err)
		}
	}
	return tx.Commit()
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (a *Adapter) ListMetrics(ctx context.Context, filter beacondb.MetricFilter) ([]beacondb.Metric, error) {
	var (
		where []string
		args  []any
	)
	if filter.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, string(filter.Kind))
	}
	if filter.Name != "" {
		where = append(where, "name = ?")
		args = append(args, filter.Name)
	}
	if filter.PeriodKind != "" {
		where = append(where, "period_kind = ?")
		args = append(args, string(filter.PeriodKind))
	}
	if filter.PeriodWindow != "" {
		where = append(where, "period_window = ?")
		args = append(args, filter.PeriodWindow)
	}
	if filter.Fingerprint != "" {
		where = append(where, "fingerprint = ?")
		args = append(args, filter.Fingerprint)
	}
	if !filter.Since.IsZero() {
		where = append(where, "period_start >= ?")
		args = append(args, filter.Since.UnixNano())
	}
	if !filter.Until.IsZero() {
		where = append(where, "period_start < ?")
		args = append(args, filter.Until.UnixNano())
	}
	if filter.ExcludeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}

	sqlStr := `
SELECT id, kind, name, period_kind, period_window, period_start, count, sum, p50, p95, p99,
       fingerprint, dimensions, dimension_hash, introduced_deploy_sha, created_at, updated_at
  FROM beacon_metrics`
	if len(where) > 0 {
		sqlStr += " WHERE " + strings.Join(where, " AND ")
	}
	sqlStr += " ORDER BY period_start ASC, id ASC"
	if filter.Limit > 0 {
		sqlStr += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := a.db.QueryContext(ctx, sqlStr, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []beacondb.Metric
	for rows.Next() {
		var (
			m                              beacondb.Metric
			kind, pk, dims                 string
			introSHA                       sql.NullString
			periodStartNS                  int64
			sumN, p50N, p95N, p99N         sql.NullFloat64
			createdNS, updatedNS           int64
		)
		if err := rows.Scan(
			&m.ID, &kind, &m.Name, &pk, &m.PeriodWindow, &periodStartNS,
			&m.Count, &sumN, &p50N, &p95N, &p99N,
			&m.Fingerprint, &dims, &m.DimensionHash, &introSHA,
			&createdNS, &updatedNS,
		); err != nil {
			return nil, err
		}
		m.Kind = beacondb.Kind(kind)
		m.PeriodKind = beacondb.PeriodKind(pk)
		if introSHA.Valid {
			m.IntroducedDeploySHA = introSHA.String
		}
		if sumN.Valid {
			v := sumN.Float64
			m.Sum = &v
		}
		if p50N.Valid {
			v := p50N.Float64
			m.P50 = &v
		}
		if p95N.Valid {
			v := p95N.Float64
			m.P95 = &v
		}
		if p99N.Valid {
			v := p99N.Float64
			m.P99 = &v
		}
		if err := unmarshalJSON([]byte(dims), &m.Dimensions); err != nil {
			return nil, err
		}
		m.PeriodStart = time.Unix(0, periodStartNS).UTC()
		m.CreatedAt = time.Unix(0, createdNS).UTC()
		m.UpdatedAt = time.Unix(0, updatedNS).UTC()
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func marshalJSON(v map[string]any) ([]byte, error) {
	if v == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(v)
}

func unmarshalJSON(b []byte, out *map[string]any) error {
	if len(b) == 0 {
		*out = nil
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if len(m) == 0 {
		*out = nil
		return nil
	}
	*out = m
	return nil
}

func nullableInt32(p *int32) any {
	if p == nil {
		return nil
	}
	return *p
}

func nowNS() int64 { return time.Now().UTC().UnixNano() }
