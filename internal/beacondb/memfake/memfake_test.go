package memfake

import (
	"testing"

	"github.com/luuuc/beacon/internal/beacondb"
)

func TestMemfakeConformance(t *testing.T) {
	beacondb.RunConformance(t, func(tb testing.TB) beacondb.Adapter {
		return New()
	})
}
