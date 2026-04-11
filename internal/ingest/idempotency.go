package ingest

import (
	"sort"
	"sync"
	"time"
)

// idempStore is an in-memory key→timestamp map with a TTL. It is the 10-min
// ring buffer mentioned in the pitch — a restart drops the window (documented
// as acceptable for v1).
//
// Sweeps happen inline on any call more than gcInterval since the last
// sweep, so there is no background goroutine to stop.
//
// A hard entry cap (maxEntries) bounds worst-case memory: a runaway client
// sending unique Idempotency-Key values at >maxEntries/ttl rps would
// otherwise grow the map without bound until the next sweep. When the cap
// trips on record, the oldest half of the entries (by timestamp) are
// evicted in one O(n) pass — cheap at the sizes we care about, and
// eviction-by-halves means we don't thrash on the cap.
type idempStore struct {
	mu         sync.Mutex
	seen       map[string]time.Time
	ttl        time.Duration
	lastGC     time.Time
	gcInterval time.Duration
	maxEntries int
	now        func() time.Time
}

// defaultIdempMaxEntries bounds the idempotency map. At 100 rps per IP and
// a 10-min TTL, steady-state is ~60k entries, so 100k leaves headroom for
// bursts; a million keys of ~80 bytes each would still only be ~80 MB, but
// we cap well below that because anything higher is almost certainly abuse.
const defaultIdempMaxEntries = 100_000

func newIdempStore(ttl time.Duration) *idempStore {
	return &idempStore{
		seen:       map[string]time.Time{},
		ttl:        ttl,
		gcInterval: time.Minute,
		maxEntries: defaultIdempMaxEntries,
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
	if s.maxEntries > 0 && len(s.seen) >= s.maxEntries {
		s.evictHalfLocked()
	}
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

// evictHalfLocked drops the oldest half of entries by timestamp. Called
// only when the entry cap trips; the sweep has already run, so surviving
// entries are all inside the TTL window. sort.Slice at 100k entries is
// on the order of tens of ms — acceptable for a cap trip that should
// itself be rare. A quickselect would shave constant factor but at the
// cost of extra code in the package namespace.
func (s *idempStore) evictHalfLocked() {
	if len(s.seen) == 0 {
		return
	}
	times := make([]time.Time, 0, len(s.seen))
	for _, t := range s.seen {
		times = append(times, t)
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	pivot := times[len(times)/2]
	for k, t := range s.seen {
		if t.Before(pivot) || t.Equal(pivot) {
			delete(s.seen, k)
		}
	}
}
