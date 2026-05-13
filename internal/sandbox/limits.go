package sandbox

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// limiterMaxIdle evicts limiters that haven't been touched in this long. Keeps
// the limiter map bounded across many slugs without losing the rate context
// for hot sites.
const limiterMaxIdle = 10 * time.Minute

type limiterEntry struct {
	limiter  *rate.Limiter
	lastUsed time.Time
}

// limiters holds per-(slug, function) token buckets. Idle entries are evicted
// on read; we don't bother with a background sweep until measurements show we
// need one.
type limiters struct {
	mu      sync.Mutex
	entries map[string]*limiterEntry
	rps     rate.Limit
	burst   int
}

func newLimiters(rps float64, burst int) *limiters {
	if rps <= 0 {
		rps = 10
	}
	if burst <= 0 {
		burst = 20
	}
	return &limiters{
		entries: map[string]*limiterEntry{},
		rps:     rate.Limit(rps),
		burst:   burst,
	}
}

// allow returns true if the (slug, name) pair has tokens available right now.
// Side effect: evicts other entries idle past limiterMaxIdle.
func (l *limiters) allow(slug, name string) bool {
	key := slug + "/" + name
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistic eviction; cheap because limiters are tiny and the map is
	// small in practice. If it ever isn't, switch to an LRU.
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
