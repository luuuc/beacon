// Package adapterfactory routes a config.DatabaseConfig to the concrete
// beacondb.Adapter backing it. Keeping the switch in one place means the
// rest of the binary never has to import each adapter package.
package adapterfactory

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luuuc/beacon/internal/beacondb"
	"github.com/luuuc/beacon/internal/beacondb/mysqladapter"
	"github.com/luuuc/beacon/internal/beacondb/pgadapter"
	"github.com/luuuc/beacon/internal/beacondb/sqliteadapter"
	"github.com/luuuc/beacon/internal/config"
)

// Open returns a concrete adapter for cfg. If cfg.Adapter is set, that wins;
// otherwise the adapter is inferred from URL scheme or from Path being set.
func Open(ctx context.Context, cfg config.DatabaseConfig) (beacondb.Adapter, error) {
	kind, err := resolveKind(cfg)
	if err != nil {
		return nil, err
	}
	switch kind {
	case "postgres":
		return pgadapter.Open(ctx, pgadapter.Config{URL: cfg.URL, Schema: cfg.Schema})
	case "mysql":
		return mysqladapter.Open(ctx, mysqladapter.Config{DSN: cfg.URL})
	case "sqlite":
		return sqliteadapter.Open(ctx, sqliteadapter.Config{Path: cfg.Path})
	default:
		return nil, fmt.Errorf("adapterfactory: unsupported adapter %q", kind)
	}
}

// ResolveKind exposes the adapter inference without opening a connection.
// Useful for config validation at boot.
func ResolveKind(cfg config.DatabaseConfig) (string, error) {
	return resolveKind(cfg)
}

func resolveKind(cfg config.DatabaseConfig) (string, error) {
	if a := strings.ToLower(strings.TrimSpace(cfg.Adapter)); a != "" {
		switch a {
		case "postgres", "postgresql":
			return "postgres", nil
		case "mysql", "mariadb":
			return "mysql", nil
		case "sqlite", "sqlite3":
			return "sqlite", nil
		default:
			return "", fmt.Errorf("adapterfactory: unknown database.adapter %q", cfg.Adapter)
		}
	}
	if cfg.URL != "" {
		switch {
		case strings.HasPrefix(cfg.URL, "postgres://"), strings.HasPrefix(cfg.URL, "postgresql://"):
			return "postgres", nil
		case strings.HasPrefix(cfg.URL, "mysql://"):
			return "", errors.New("adapterfactory: set database.adapter: mysql explicitly — go-sql-driver uses a native DSN, not a mysql:// URL")
		}
	}
	if cfg.Path != "" {
		return "sqlite", nil
	}
	return "", errors.New("adapterfactory: database config is empty — set database.url or database.path or database.adapter")
}
