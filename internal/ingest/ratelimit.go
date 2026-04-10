package ingest

import (
	"sync"
	"time"
)

// rateLimiter is a per-IP token-bucket limiter. It is intentionally simple:
// no sharding, no background sweeper, one mutex for everything. Beacon's
// expected fan-in is one or two processes talking to it — not an open
// internet listener.
type rateLimiter struct {
	mu     sync.Mutex
	rate   float64 // tokens added per second
	burst  float64 // bucket capacity
	ips    map[string]*bucket
	maxIPs int
	now    func() time.Time
}

type bucket struct {
	tokens  float64
	updated time.Time
}

func newRateLimiter(ratePerSec, burst float64) *rateLimiter {
	return &rateLimiter{
		rate:   ratePerSec,
		burst:  burst,
		ips:    map[string]*bucket{},
		maxIPs: 10000,
		now:    time.Now,
	}
}

// allow reports whether a request from ip is permitted right now. It also
// computes the retry-after hint (seconds until at least one token is
// available), which the handler passes through as the Retry-After header
// on 429.
func (r *rateLimiter) allow(ip string) (ok bool, retryAfterSeconds int) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Unbounded maps are a slow leak. When the table fills, drop the state —
	// every known caller gets a fresh bucket, which is effectively a free
	// re-grant. For beacon's single-tenant deployment that is fine.
	if len(r.ips) >= r.maxIPs {
		r.ips = map[string]*bucket{}
	}

	now := r.now()
	b, ok := r.ips[ip]
	if !ok {
		b = &bucket{tokens: r.burst, updated: now}
		r.ips[ip] = b
	} else {
		elapsed := now.Sub(b.updated).Seconds()
		b.tokens += elapsed * r.rate
		if b.tokens > r.burst {
			b.tokens = r.burst
		}
		b.updated = now
	}
	if b.tokens < 1 {
		deficit := 1 - b.tokens
		retry := int(deficit/r.rate) + 1
		if retry < 1 {
			retry = 1
		}
		return false, retry
	}
	b.tokens--
	return true, 0
}
