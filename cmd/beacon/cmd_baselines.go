package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/luuuc/beacon/internal/beacondb"
)

// cmdBaselines dispatches `beacon baselines <subcommand>`.
func cmdBaselines(args []string, log *slog.Logger, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "beacon baselines: subcommand required (export)")
		return 2
	}
	switch args[0] {
	case "export":
		return cmdBaselinesExport(args[1:], log, stderr)
	default:
		fmt.Fprintf(stderr, "beacon baselines: unknown subcommand %q\n", args[0])
		return 2
	}
}

func cmdBaselinesExport(args []string, _ *slog.Logger, stderr io.Writer) int {
	fs := flag.NewFlagSet("baselines export", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", "", "path to beacon.yml")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, adapter, err := openAdapterForCLI(ctx, *configPath)
	if err != nil {
		fmt.Fprintf(stderr, "beacon baselines export: %v\n", err)
		return 1
	}
	defer func() { _ = adapter.Close() }()

	if err := adapter.Migrate(ctx); err != nil {
		fmt.Fprintf(stderr, "beacon baselines export: migrate: %v\n", err)
		return 1
	}

	rows, err := adapter.ListMetrics(ctx, beacondb.MetricFilter{
		PeriodKind: beacondb.PeriodBaseline,
	})
	if err != nil {
		fmt.Fprintf(stderr, "beacon baselines export: list baselines: %v\n", err)
		return 1
	}

	if err := writeBaselinesSQL(os.Stdout, rows); err != nil {
		fmt.Fprintf(stderr, "beacon baselines export: write: %v\n", err)
		return 1
	}
	fmt.Fprintf(stderr, "exported %d baseline row(s)\n", len(rows))
	return 0
}

// writeBaselinesSQL emits one INSERT ... ON CONFLICT DO NOTHING statement per
// baseline row. The dialect is PostgreSQL-compatible; MySQL/SQLite users
// will need to rewrite `ON CONFLICT DO NOTHING` → `IGNORE`/`OR IGNORE`. We
// ship PG because that's the primary production target.
func writeBaselinesSQL(out io.Writer, rows []beacondb.Metric) error {
	header := `-- beacon baselines export
-- Dialect: PostgreSQL
-- Restore with:  psql -f baselines.sql <database>
--
`
	if _, err := io.WriteString(out, header); err != nil {
		return err
	}
	for _, m := range rows {
		stmt, err := baselineInsertSQL(m)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(out, stmt+"\n"); err != nil {
			return err
		}
	}
	return nil
}

func baselineInsertSQL(m beacondb.Metric) (string, error) {
	dimsJSON := "{}"
	if m.Dimensions != nil {
		b, err := json.Marshal(m.Dimensions)
		if err != nil {
			return "", fmt.Errorf("marshal dimensions: %w", err)
		}
		dimsJSON = string(b)
	}
	var b strings.Builder
	b.WriteString("INSERT INTO beacon_metrics (")
	b.WriteString("kind, name, period_kind, period_window, period_start, count, sum, p50, p95, p99, fingerprint, dimensions, dimension_hash")
	b.WriteString(") VALUES (")
	b.WriteString(sqlString(string(m.Kind)))
	b.WriteString(", ")
	b.WriteString(sqlString(m.Name))
	b.WriteString(", ")
	b.WriteString(sqlString(string(m.PeriodKind)))
	b.WriteString(", ")
	b.WriteString(sqlString(m.PeriodWindow))
	b.WriteString(", ")
	b.WriteString(sqlTimestamp(m.PeriodStart))
	b.WriteString(", ")
	b.WriteString(fmt.Sprintf("%d", m.Count))
	b.WriteString(", ")
	b.WriteString(sqlFloatPtr(m.Sum))
	b.WriteString(", ")
	b.WriteString(sqlFloatPtr(m.P50))
	b.WriteString(", ")
	b.WriteString(sqlFloatPtr(m.P95))
	b.WriteString(", ")
	b.WriteString(sqlFloatPtr(m.P99))
	b.WriteString(", ")
	b.WriteString(sqlString(m.Fingerprint))
	b.WriteString(", ")
	b.WriteString(sqlString(dimsJSON))
	b.WriteString("::jsonb, ")
	b.WriteString(sqlString(m.DimensionHash))
	b.WriteString(") ON CONFLICT DO NOTHING;")
	return b.String(), nil
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

func sqlTimestamp(t time.Time) string {
	return "'" + t.UTC().Format(time.RFC3339Nano) + "'"
}

func sqlFloatPtr(p *float64) string {
	if p == nil {
		return "NULL"
	}
	return fmt.Sprintf("%v", *p)
}
