// Package ratelimit is a per-key token-bucket limiter.
//
// It exists so the OAuth package can cap its unauthenticated registration
// endpoint without dragging a host application's own limiter along. Under
// auth/internal/ so it stays an implementation detail: consumers get the
// rate limiting, not another type to maintain.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiterMaxIdle evicts buckets untouched for this long, keeping the map
// bounded across many callers without losing the rate context for an active
// one.
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

// New returns a Limiter allowing rps sustained requests per key with the
// given burst. Non-positive values fall back to conservative defaults (0.2 rps,
// burst 5) suited to an open photo-upload link.
func New(rps float64, burst int) *Limiter {
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

// Allow reports whether key (typically a client IP) has a token
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
