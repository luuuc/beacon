package mysqladapter

import (
	"context"
	"database/sql"
	"os"
	"testing"

	"github.com/go-sql-driver/mysql"

	"github.com/luuuc/beacon/internal/beacondb"
)

// TestMysqladapterConformance runs the shared Adapter contract against a
// real MySQL / MariaDB instance. Skips if BEACON_TEST_MYSQL_URL is unset.
//
// Each subtest gets its own database (MySQL's "database" is the rough
// equivalent of a Postgres schema) so state is isolated without TRUNCATE.
func TestMysqladapterConformance(t *testing.T) {
	rootDSN := os.Getenv("BEACON_TEST_MYSQL_URL")
	if rootDSN == "" {
		t.Skip("BEACON_TEST_MYSQL_URL unset; skipping MySQL conformance")
	}

	var counter int
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		counter++
		dbName := uniqueDBName(tb, counter)

		// Use a short-lived root connection (empty DBName) to create the
		// per-subtest database, then switch DSN to that database.
		rootCfg, err := mysql.ParseDSN(rootDSN)
		if err != nil {
			tb.Fatalf("parse root DSN: %v", err)
		}
		originalDB := rootCfg.DBName
		rootCfg.DBName = ""
		rootCfg.MultiStatements = true

		rootDB, err := sql.Open("mysql", rootCfg.FormatDSN())
		if err != nil {
			tb.Fatalf("open root: %v", err)
		}
		if _, err := rootDB.Exec("CREATE DATABASE `" + dbName + "` CHARACTER SET utf8mb4 COLLATE utf8mb4_bin"); err != nil {
			_ = rootDB.Close()
			tb.Fatalf("create db %s: %v", dbName, err)
		}
		_ = rootDB.Close()

		perCfg := rootCfg
		perCfg.DBName = dbName
		a, err := Open(context.Background(), Config{DSN: perCfg.FormatDSN()})
		if err != nil {
			tb.Fatalf("Open: %v", err)
		}
		tb.Cleanup(func() {
			_ = a.Close()
			cleanupCfg := rootCfg
			cleanupCfg.DBName = originalDB // may be empty; that's fine
			clean, err := sql.Open("mysql", cleanupCfg.FormatDSN())
			if err == nil {
				_, _ = clean.Exec("DROP DATABASE IF EXISTS `" + dbName + "`")
				_ = clean.Close()
			}
		})
		return a
	})
}

func uniqueDBName(tb testing.TB, n int) string {
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
