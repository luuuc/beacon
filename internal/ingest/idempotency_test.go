package ingest

import (
	"fmt"
	"testing"
	"time"
)

func TestIdempStoreEntryCap(t *testing.T) {
	s := newIdempStore(10 * time.Minute)
	cap := 100
	s.maxEntries = cap

	base := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	clock := base
	s.now = func() time.Time { return clock }

	// Fill past the cap. Each entry gets a distinct timestamp so we can
	// tell which half survived.
	for i := 0; i < cap*3; i++ {
		s.record(fmt.Sprintf("k%d", i))
		clock = clock.Add(time.Millisecond)
	}

	if n := len(s.seen); n > cap {
		t.Fatalf("len(seen) = %d, want ≤ %d after cap trip", n, cap)
	}
	if n := len(s.seen); n == 0 {
		t.Fatal("len(seen) = 0, eviction nuked everything")
	}

	// The most recent key should still be present.
	if !s.wasSeen(fmt.Sprintf("k%d", cap*3-1)) {
		t.Errorf("newest key evicted")
	}
	// The oldest key should be gone.
	if s.wasSeen("k0") {
		t.Errorf("oldest key survived eviction")
	}
}

func TestIdempStoreSweepHonoursTTL(t *testing.T) {
	s := newIdempStore(10 * time.Minute)
	clock := time.Date(2026, 4, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return clock }

	s.record("old")
	clock = clock.Add(11 * time.Minute) // past ttl + past gcInterval
	if s.wasSeen("old") {
		t.Fatal("expired key reported as seen")
	}
	if _, still := s.seen["old"]; still {
		t.Errorf("sweep did not delete expired key")
	}
}
