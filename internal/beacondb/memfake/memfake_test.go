package memfake

import (
	"testing"

	"github.com/luuuc/beacon/internal/beacondb"
)

// TestMemfakeConformance is the only test file in this package: it runs
// the shared beacondb.RunConformance matrix against a fresh Fake. Every
// Adapter method is exercised there, which is why there are no per-method
// unit tests. See the package doc comment.
func TestMemfakeConformance(t *testing.T) {
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		return New()
	})
}
