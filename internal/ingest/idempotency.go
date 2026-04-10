package ingest

import (
	"sync"
	"time"
)

// idempStore is an in-memory key→timestamp map with a TTL. It is the 10-min
// ring buffer mentioned in the pitch — a restart drops the window (documented
// as acceptable for v1).
//
// Sweeps happen inline on any call more than gcInterval since the last
// sweep, so there is no background goroutine to stop.
type idempStore struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	ttl        time.Duration
	lastGC     time.Time
	gcInterval time.Duration
	now        func() time.Time
}

func newIdempStore(ttl time.Duration) *idempStore {
	return &idempStore{
		seen:       map[string]time.Time{},
		ttl:        ttl,
		gcInterval: time.Minute,
		now:        time.Now,
	}
}

// wasSeen reports whether the key is within the TTL. It does not record.
// Split from record so that a request that fails validation does not burn
// the idempotency key — the client can retry with the same key after fixing
// the bug.
func (s *idempStore) wasSeen(key string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	t, ok := s.seen[key]
	return ok && s.now().Sub(t) <= s.ttl
}

// record stores the key under the current timestamp.
func (s *idempStore) record(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	s.seen[key] = s.now()
}

func (s *idempStore) sweepLocked() {
	now := s.now()
	if now.Sub(s.lastGC) <= s.gcInterval {
		return
	}
	for k, t := range s.seen {
		if now.Sub(t) > s.ttl {
			delete(s.seen, k)
		}
	}
	s.lastGC = now
}
