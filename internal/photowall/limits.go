package photowall

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// This is a standalone per-key token-bucket limiter for the visitor upload
// endpoint. It mirrors internal/sandbox's limiter structurally, but that one is
// unexported and keyed for functions; the upload path keys on (slug, client IP)
// and lives outside internal/server, so it needs its own copy here.

// limiterMaxIdle evicts buckets untouched for this long, keeping the map
// bounded across many uploaders without losing the rate context for an
// actively-uploading crowd.
const limiterMaxIdle = 10 * time.Minute

type limiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// Limiter holds per-key token buckets. Idle entries are evicted opportunistically
// on read. Safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	rps     rate.Limit
	burst   int
}

// NewLimiter returns a Limiter allowing rps sustained requests per key with the
// given burst. Non-positive values fall back to conservative defaults (0.2 rps,
// burst 5) suited to an open photo-upload link.
func NewLimiter(rps float64, burst int) *Limiter {
	if rps <= 0 {
		rps = 0.2
	}
	if burst <= 0 {
		burst = 5
	}
	return &Limiter{
		entries: map[string]*limiterEntry{},
		rps:     rate.Limit(rps),
		burst:   burst,
	}
}

// Allow reports whether key (typically slug + "|" + clientIP) has a token
// available right now, consuming one if so. Side effect: evicts other buckets
// idle past limiterMaxIdle.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	cutoff := now.Add(-limiterMaxIdle)
	for k, e := range l.entries {
		if e.lastUsed.Before(cutoff) && k != key {
			delete(l.entries, k)
		}
	}

	e, ok := l.entries[key]
	if !ok {
		e = &limiterEntry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.entries[key] = e
	}
	e.lastUsed = now
	return e.limiter.Allow()
}
