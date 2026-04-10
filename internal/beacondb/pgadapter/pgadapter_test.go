package pgadapter

import (
	"context"
	"os"
	"testing"

	"github.com/luuuc/beacon/internal/beacondb"
)

// TestPgadapterConformance runs the shared Adapter contract against a real
// PostgreSQL instance. Skips if BEACON_TEST_POSTGRES_URL is unset.
//
// Each subtest gets its own temporary schema so state is isolated without
// having to truncate between runs.
func TestPgadapterConformance(t *testing.T) {
	url := os.Getenv("BEACON_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("BEACON_TEST_POSTGRES_URL unset; skipping Postgres conformance")
	}

	var schemaCounter int
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		schemaCounter++
		schema := uniqueSchemaName(tb, schemaCounter)
		a, err := Open(context.Background(), Config{URL: url, Schema: schema})
		if err != nil {
			tb.Fatalf("Open: %v", err)
		}
		tb.Cleanup(func() {
			ctx := context.Background()
			_, _ = a.pool.Exec(ctx, `DROP SCHEMA IF EXISTS `+quoteIdent(schema)+` CASCADE`)
		})
		return a
	})
}

func uniqueSchemaName(tb testing.TB, n int) string {
	// Postgres identifiers are 63 bytes max; subtest names can be long.
	// We only need uniqueness across the conformance run.
	name := "beacon_test_" + sanitize(tb.Name())
	if len(name) > 50 {
		name = name[:50]
	}
	return name + "_" + itoa(n)
}

func sanitize(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b = append(b, byte(r))
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func quoteIdent(s string) string {
	return `"` + s + `"`
}
