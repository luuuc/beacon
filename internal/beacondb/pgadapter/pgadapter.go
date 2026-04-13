// Package pgadapter is the PostgreSQL implementation of beacondb.Adapter.
//
// Connection pool: pgx/v5's pgxpool. Schema isolation: if a schema name is
// configured, the adapter creates it on first connect and sets search_path
// so every subsequent statement targets that schema. Migrations are embedded
// SQL files under migrations/, tracked in beacon_schema_migrations.
package pgadapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/luuuc/beacon/internal/beacondb"
)

// Config is the subset of beacon config that pgadapter actually consumes.
type Config struct {
	URL      string // postgres:// DSN
	Schema   string // optional; created if missing, used as search_path
	MaxConns int    // pgx pool cap; 0 leaves pgx's default (derived from the DSN or 4).
}

type Adapter struct {
	pool *pgxpool.Pool
}

var schemaIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Open builds a pool against cfg.URL. If cfg.Schema is set, every connection
// creates the schema if missing and sets its search_path.
func Open(ctx context.Context, cfg Config) (*Adapter, error) {
	if cfg.URL == "" {
		return nil, errors.New("pgadapter: URL is required")
	}
	poolCfg, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("pgadapter: parse URL: %w", err)
	}

	if cfg.MaxConns > 0 {
		poolCfg.MaxConns = int32(cfg.MaxConns)
	}

	if cfg.Schema != "" {
		if !schemaIdent.MatchString(cfg.Schema) {
			return nil, fmt.Errorf("pgadapter: schema %q must match [A-Za-z_][A-Za-z0-9_]*", cfg.Schema)
		}
		schema := cfg.Schema
		poolCfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, err := conn.Exec(ctx, fmt.Sprintf(`CREATE SCHEMA IF NOT EXISTS %q`, schema)); err != nil {
				return fmt.Errorf("create schema: %w", err)
			}
			if _, err := conn.Exec(ctx, fmt.Sprintf(`SET search_path TO %q`, schema)); err != nil {
				return fmt.Errorf("set search_path: %w", err)
			}
			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pgadapter: connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgadapter: ping: %w", err)
	}
	return &Adapter{pool: pool}, nil
}

func (a *Adapter) Ping(ctx context.Context) error {
	return a.pool.Ping(ctx)
}

func (a *Adapter) Migrate(ctx context.Context) error {
	return applyMigrations(ctx, a.pool)
}

func (a *Adapter) MigrationsApplied(ctx context.Context) (bool, error) {
	return migrationsApplied(ctx, a.pool)
}

func (a *Adapter) Close() error {
	a.pool.Close()
	return nil
}

// ---------------------------------------------------------------------------
// beacon_events writes + reads
// ---------------------------------------------------------------------------

const insertEventSQL = `
INSERT INTO beacon_events
  (kind, name, actor_type, actor_id, duration_ms, status, fingerprint, properties, context, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9, COALESCE($10, NOW()))
RETURNING id`

func (a *Adapter) InsertEvents(ctx context.Context, events []beacondb.Event) ([]int64, error) {
	if len(events) == 0 {
		return nil, nil
	}

	// Pipeline the whole batch into a single round-trip via pgx.Batch.
	// Transaction semantics are kept so a mid-batch failure rolls everything
	// back — matches the Adapter contract ("batch is atomic").
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for i, e := range events {
		props, err := marshalJSON(e.Properties)
		if err != nil {
			return nil, fmt.Errorf("event[%d] properties: %w", i, err)
		}
		cx, err := marshalJSON(e.Context)
		if err != nil {
			return nil, fmt.Errorf("event[%d] context: %w", i, err)
		}
		var createdAt any
		if !e.CreatedAt.IsZero() {
			createdAt = e.CreatedAt
		}
		batch.Queue(insertEventSQL,
			string(e.Kind), e.Name, e.ActorType, e.ActorID,
			nullableInt32(e.DurationMs), nullableInt32(e.Status),
			e.Fingerprint, props, cx, createdAt,
		)
	}

	br := tx.SendBatch(ctx, batch)
	ids := make([]int64, len(events))
	for i := range events {
		if err := br.QueryRow().Scan(&ids[i]); err != nil {
			_ = br.Close()
			return nil, fmt.Errorf("insert event[%d]: %w", i, err)
		}
	}
	if err := br.Close(); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Adapter) ListEvents(ctx context.Context, filter beacondb.EventFilter) ([]beacondb.Event, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if filter.Kind != "" {
		add("kind = $%d", string(filter.Kind))
	}
	if filter.Name != "" {
		add("name = $%d", filter.Name)
	}
	if filter.Fingerprint != "" {
		add("fingerprint = $%d", filter.Fingerprint)
	}
	if !filter.Since.IsZero() {
		add("created_at >= $%d", filter.Since)
	}
	if !filter.Until.IsZero() {
		add("created_at < $%d", filter.Until)
	}
	sql := `
SELECT id, kind, name, actor_type, actor_id, duration_ms, status,
       fingerprint, properties, context, created_at
  FROM beacon_events`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY created_at ASC, id ASC"
	if filter.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []beacondb.Event
	for rows.Next() {
		var e beacondb.Event
		var kind string
		var dur, status *int32
		var propsBytes, ctxBytes []byte
		if err := rows.Scan(
			&e.ID, &kind, &e.Name, &e.ActorType, &e.ActorID,
			&dur, &status, &e.Fingerprint, &propsBytes, &ctxBytes, &e.CreatedAt,
		); err != nil {
			return nil, err
		}
		e.Kind = beacondb.Kind(kind)
		e.DurationMs = dur
		e.Status = status
		if err := unmarshalJSON(propsBytes, &e.Properties); err != nil {
			return nil, err
		}
		if err := unmarshalJSON(ctxBytes, &e.Context); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func (a *Adapter) DeleteEventsOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := a.pool.Exec(ctx, `DELETE FROM beacon_events WHERE created_at < $1`, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

func (a *Adapter) DeleteEventsByKindOlderThan(ctx context.Context, kind beacondb.Kind, cutoff time.Time) (int64, error) {
	tag, err := a.pool.Exec(ctx, `DELETE FROM beacon_events WHERE kind = $1 AND created_at < $2`, string(kind), cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// beacon_metrics writes + reads
// ---------------------------------------------------------------------------

const upsertMetricSQL = `
INSERT INTO beacon_metrics
  (kind, name, period_kind, period_window, period_start, count, sum, p50, p95, p99,
   fingerprint, dimensions, dimension_hash, created_at, updated_at)
VALUES
  ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13, NOW(), NOW())
ON CONFLICT (kind, name, period_kind, period_window, period_start, fingerprint, dimension_hash)
DO UPDATE SET
  count      = EXCLUDED.count,
  sum        = EXCLUDED.sum,
  p50        = EXCLUDED.p50,
  p95        = EXCLUDED.p95,
  p99        = EXCLUDED.p99,
  dimensions = EXCLUDED.dimensions,
  updated_at = NOW()`

func (a *Adapter) UpsertMetrics(ctx context.Context, metrics []beacondb.Metric) error {
	if len(metrics) == 0 {
		return nil
	}
	tx, err := a.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	batch := &pgx.Batch{}
	for i, m := range metrics {
		dims, err := marshalJSON(m.Dimensions)
		if err != nil {
			return fmt.Errorf("metric[%d] dimensions: %w", i, err)
		}
		batch.Queue(upsertMetricSQL,
			string(m.Kind), m.Name, string(m.PeriodKind), m.PeriodWindow, m.PeriodStart,
			m.Count, m.Sum, m.P50, m.P95, m.P99,
			m.Fingerprint, dims, m.DimensionHash,
		)
	}
	br := tx.SendBatch(ctx, batch)
	for i := range metrics {
		if _, err := br.Exec(); err != nil {
			_ = br.Close()
			return fmt.Errorf("upsert metric[%d]: %w", i, err)
		}
	}
	if err := br.Close(); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (a *Adapter) ListMetrics(ctx context.Context, filter beacondb.MetricFilter) ([]beacondb.Metric, error) {
	var (
		where []string
		args  []any
	)
	add := func(cond string, val any) {
		args = append(args, val)
		where = append(where, fmt.Sprintf(cond, len(args)))
	}
	if filter.Kind != "" {
		add("kind = $%d", string(filter.Kind))
	}
	if filter.Name != "" {
		add("name = $%d", filter.Name)
	}
	if filter.PeriodKind != "" {
		add("period_kind = $%d", string(filter.PeriodKind))
	}
	if filter.PeriodWindow != "" {
		add("period_window = $%d", filter.PeriodWindow)
	}
	if filter.Fingerprint != "" {
		add("fingerprint = $%d", filter.Fingerprint)
	}
	if !filter.Since.IsZero() {
		add("period_start >= $%d", filter.Since)
	}
	if !filter.Until.IsZero() {
		add("period_start < $%d", filter.Until)
	}

	sql := `
SELECT id, kind, name, period_kind, period_window, period_start, count, sum, p50, p95, p99,
       fingerprint, dimensions, dimension_hash, created_at, updated_at
  FROM beacon_metrics`
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	sql += " ORDER BY period_start ASC, id ASC"
	if filter.Limit > 0 {
		sql += fmt.Sprintf(" LIMIT %d", filter.Limit)
	}

	rows, err := a.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []beacondb.Metric
	for rows.Next() {
		var m beacondb.Metric
		var kind, pk string
		var dims []byte
		if err := rows.Scan(
			&m.ID, &kind, &m.Name, &pk, &m.PeriodWindow, &m.PeriodStart,
			&m.Count, &m.Sum, &m.P50, &m.P95, &m.P99,
			&m.Fingerprint, &dims, &m.DimensionHash,
			&m.CreatedAt, &m.UpdatedAt,
		); err != nil {
			return nil, err
		}
		m.Kind = beacondb.Kind(kind)
		m.PeriodKind = beacondb.PeriodKind(pk)
		if err := unmarshalJSON(dims, &m.Dimensions); err != nil {
			return nil, err
		}
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
