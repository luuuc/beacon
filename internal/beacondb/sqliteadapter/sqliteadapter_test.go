package sqliteadapter

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/luuuc/beacon/internal/beacondb"
)

func TestSqliteadapterConformance(t *testing.T) {
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		dir := tb.TempDir()
		path := filepath.Join(dir, "beacon.db")
		a, err := Open(context.Background(), Config{Path: path})
		if err != nil {
			tb.Fatalf("Open: %v", err)
		}
		return a
	})
}
