package mysqladapter

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type migration struct {
	version int
	name    string
	sql     string
}

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

func ensureMigrationsTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS beacon_schema_migrations (
  version    INT        NOT NULL PRIMARY KEY,
  applied_at BIGINT     NOT NULL
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`)
	return err
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT version FROM beacon_schema_migrations`)
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

func applyMigrations(ctx context.Context, db *sql.DB) error {
	if err := ensureMigrationsTable(ctx, db); err != nil {
		return fmt.Errorf("ensure migrations table: %w", err)
	}
	applied, err := appliedVersions(ctx, db)
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
		if err := applyOne(ctx, db, m); err != nil {
			return fmt.Errorf("apply migration %03d (%s): %w", m.version, m.name, err)
		}
	}
	return nil
}

// applyOne runs a migration file's statements. MySQL's driver requires one
// statement per Exec unless multiStatements=true is set on the DSN (we do
// set it in Open). Even so, we don't wrap the migration in a transaction
// because MySQL DDL is auto-committed and a BEGIN/COMMIT here would be a
// lie — the tracking insert goes in its own transaction instead.
func applyOne(ctx context.Context, db *sql.DB, m migration) error {
	if _, err := db.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT IGNORE INTO beacon_schema_migrations (version, applied_at) VALUES (?, ?)`,
		m.version, nowNS(),
	)
	return err
}

func migrationsApplied(ctx context.Context, db *sql.DB) (bool, error) {
	all, err := loadMigrations()
	if err != nil {
		return false, err
	}
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		// Missing table → not migrated.
		return false, nil
	}
	for _, m := range all {
		if !applied[m.version] {
			return false, nil
		}
	}
	return true, nil
}
