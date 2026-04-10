package pgadapter

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

// loadMigrations reads every migrations/NNN_*.sql file, sorted ascending by
// the NNN prefix. Filenames must start with three digits followed by an
// underscore.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix := strings.SplitN(e.Name(), "_", 2)
		if len(prefix) != 2 || len(prefix[0]) != 3 {
			return nil, fmt.Errorf("migration filename %q does not match NNN_name.sql", e.Name())
		}
		v, err := strconv.Atoi(prefix[0])
		if err != nil {
			return nil, fmt.Errorf("migration %q: version parse: %w", e.Name(), err)
		}
		raw, err := fs.ReadFile(migrationsFS, path.Join("migrations", e.Name()))
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: e.Name(), sql: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	return out, nil
}

// ensureMigrationsTable creates beacon_schema_migrations if missing.
func ensureMigrationsTable(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS beacon_schema_migrations (
  version    INTEGER PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`)
	return err
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int]bool, error) {
	rows, err := pool.Query(ctx, `SELECT version FROM beacon_schema_migrations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// applyMigrations runs every pending migration in a transaction per version.
func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if err := ensureMigrationsTable(ctx, pool); err != nil {
		return fmt.Errorf("ensure migrations table: %w", err)
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return fmt.Errorf("read applied versions: %w", err)
	}
	all, err := loadMigrations()
	if err != nil {
		return err
	}
	for _, m := range all {
		if applied[m.version] {
			continue
		}
		if err := applyOne(ctx, pool, m); err != nil {
			return fmt.Errorf("apply migration %03d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO beacon_schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`,
		m.version,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// migrationsApplied reports whether every known migration is in the table.
func migrationsApplied(ctx context.Context, pool *pgxpool.Pool) (bool, error) {
	all, err := loadMigrations()
	if err != nil {
		return false, err
	}
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		// If the table doesn't exist yet, we're clearly not migrated.
		return false, nil
	}
	for _, m := range all {
		if !applied[m.version] {
			return false, nil
		}
	}
	return true, nil
}
