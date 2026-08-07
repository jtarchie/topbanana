package server

import (
	"context"
	"crypto/tls"
	"errors"
	"strings"
	"sync"
	"testing"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// fakeCache is an in-memory autocert.Cache that counts writes, standing in for
// the S3-backed one. The write count is the point: coalescing is what keeps a
// per-handshake diagnostic from becoming a per-handshake S3 PUT.
type fakeCache struct {
	mu     sync.Mutex
	data   map[string][]byte
	writes map[string]int
}

func newFakeCache() *fakeCache {
	return &fakeCache{data: map[string][]byte{}, writes: map[string]int{}}
}

func (c *fakeCache) Get(_ context.Context, key string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.data[key]
	if !ok {
		return nil, autocert.ErrCacheMiss
	}
	return v, nil
}

func (c *fakeCache) Put(_ context.Context, key string, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = data
	c.writes[key]++
	return nil
}

func (c *fakeCache) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.data, key)
	return nil
}

func (c *fakeCache) writeCount(key string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writes[key]
}

// allowHosts builds a manager whose HostPolicy admits only the listed hosts,
// mirroring the real one (which consults the site registry).
func allowHosts(hosts ...string) *autocert.Manager {
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[h] = true
	}
	return &autocert.Manager{HostPolicy: func(_ context.Context, host string) error {
		if allowed[host] {
			return nil
		}
		return errors.New("host not configured")
	}}
}

// The failure record has to outlive the process and reach every instance:
// without it a permanently broken domain reports "pending" — "nobody has tried
// yet" — after any restart, which is not just vaguer but wrong.
func TestCertTracker_AttemptSurvivesRestartAndInstances(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "knowhere.live"

	cache := newFakeCache()
	first := NewCertTracker(allowHosts(host), cache)
	first.record(ctx, host, errors.New("acme: authorization failed: CAA record forbids issuance"))

	// A second tracker over the same cache is both "after a restart" and
	// "a different machine" — it shares nothing but the cache.
	second := NewCertTracker(allowHosts(host), cache)
	got, ok := second.LastAttempt(ctx, host)
	if !ok {
		t.Fatal("second tracker sees no attempt; the record did not persist")
	}
	if !strings.Contains(got.Err, "CAA record forbids issuance") {
		t.Errorf("persisted error = %q; want the verbatim ACME text", got.Err)
	}
	if got.At.IsZero() {
		t.Error("persisted attempt lost its timestamp")
	}
}

// A success must overwrite a stored failure. Otherwise a domain that was fixed
// reports "issued" while still carrying the old error alongside it.
func TestCertTracker_SuccessClearsStoredFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "knowhere.live"

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts(host), cache)
	tracker.record(ctx, host, errors.New("acme: dns problem"))
	tracker.record(ctx, host, nil)

	fresh := NewCertTracker(allowHosts(host), cache)
	got, ok := fresh.LastAttempt(ctx, host)
	if !ok {
		t.Fatal("no attempt recorded")
	}
	if got.Err != "" {
		t.Errorf("stored error = %q; want it cleared by the successful attempt", got.Err)
	}
}

// record runs on every handshake. An unchanged outcome must not write every
// time, but a *changed* one has to land immediately — that's the transition
// an operator is waiting to see.
func TestCertTracker_CoalescesRepeatedOutcomes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "knowhere.live"
	key := certStatusKeyPrefix + host

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts(host), cache)

	sameErr := errors.New("acme: dns problem")
	for range 50 {
		tracker.record(ctx, host, sameErr)
	}
	if n := cache.writeCount(key); n != 1 {
		t.Errorf("identical failures wrote %d times; want 1 within the refresh window", n)
	}

	tracker.record(ctx, host, errors.New("acme: unauthorized"))
	if n := cache.writeCount(key); n != 2 {
		t.Errorf("a changed error wrote %d times total; want it persisted immediately (2)", n)
	}
}

// failingCache rejects writes, standing in for an S3 blip or a cancelled
// context. Reads still work so the "nothing was stored" assertion is real.
type failingCache struct{ *fakeCache }

func (failingCache) Put(context.Context, string, []byte) error {
	return errors.New("s3 unavailable")
}

// A write that fails must not count as persisted. It used to: persistedAt
// advanced before the Put ran, so the failure was both dropped and coalesced
// away for the whole refresh window.
func TestCertTracker_FailedPersistIsRetried(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "knowhere.live"

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts(host), failingCache{cache})
	sameErr := errors.New("acme: dns problem")
	tracker.record(ctx, host, sameErr)
	if len(cache.data) != 0 {
		t.Fatalf("failing cache stored something: %v", cache.data)
	}

	// The very next identical attempt must try again rather than be deduped
	// against a write that never landed. Swap in a working cache to observe it.
	working := NewCertTracker(allowHosts(host), cache)
	working.mu.Lock()
	working.attempts[host] = tracker.attempts[host]
	working.mu.Unlock()
	working.record(ctx, host, sameErr)

	if n := cache.writeCount(certStatusKeyPrefix + host); n != 1 {
		t.Errorf("retry after a failed persist wrote %d times; want 1", n)
	}
}

// Hostnames are case-insensitive and SNI arrives raw off the wire. Without
// normalization the same domain keeps two records with independent coalescing
// clocks, both writing to the same lowercased object.
func TestCertTracker_NormalizesHostCase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts("knowhere.live", "Knowhere.Live"), cache)
	tracker.record(ctx, "Knowhere.Live", errors.New("acme: dns problem"))

	got, ok := tracker.LastAttempt(ctx, "knowhere.live")
	if !ok {
		t.Fatal("lowercase lookup missed a record stored from mixed-case SNI")
	}
	if got.Err == "" {
		t.Error("record lost its error across the case change")
	}
	if len(tracker.attempts) != 1 {
		t.Errorf("attempts map holds %d entries; want one per host", len(tracker.attempts))
	}
}

// The ACME server validates a TLS-ALPN-01 challenge over a real handshake that
// succeeds by returning the challenge certificate. Recording it would clear a
// stored failure mid-issuance.
func TestCertTracker_IgnoresALPNChallengeHandshake(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const host = "knowhere.live"

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts(host), cache)
	tracker.record(ctx, host, errors.New("acme: authorization failed"))

	if isACMEChallenge(&tls.ClientHelloInfo{ServerName: host}) {
		t.Error("an ordinary handshake must not look like a challenge")
	}
	challenge := &tls.ClientHelloInfo{ServerName: host, SupportedProtos: []string{acme.ALPNProto}}
	if !isACMEChallenge(challenge) {
		t.Fatal("an acme-tls/1 handshake must be recognized as a challenge")
	}

	// The stored failure survives, because the challenge handshake is skipped.
	got, ok := tracker.LastAttempt(ctx, host)
	if !ok || got.Err == "" {
		t.Errorf("stored failure = %+v; want it preserved through validation", got)
	}
}

// SNI is attacker-controlled and reaches record() before anything validates
// it. Recording unadmitted hosts would let an unauthenticated scanner grow the
// map without bound and drive one S3 write per novel hostname.
func TestCertTracker_IgnoresHostsItDoesNotServe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cache := newFakeCache()
	tracker := NewCertTracker(allowHosts("knowhere.live"), cache)
	tracker.record(ctx, "attacker.example", errors.New("acme: host not configured"))

	if _, ok := tracker.LastAttempt(ctx, "attacker.example"); ok {
		t.Error("recorded an attempt for a host the policy rejects")
	}
	if len(cache.data) != 0 {
		t.Errorf("unadmitted host produced cache writes: %v", cache.data)
	}
	if len(tracker.attempts) != 0 {
		t.Errorf("unadmitted host grew the in-memory map: %v", tracker.attempts)
	}
}

// The host becomes part of an S3 object key, so anything path-shaped must not
// build a key at all.
func TestCertStatusKey_RejectsPathShapedHosts(t *testing.T) {
	t.Parallel()

	for _, host := range []string{"", "../../etc/passwd", `a\b`, "a/b", "x..y", "ok\x00", strings.Repeat("a", 254)} {
		if key, ok := certStatusKey(host); ok {
			t.Errorf("certStatusKey(%q) = %q, true; want rejection", host, key)
		}
	}
	key, ok := certStatusKey("Knowhere.Live")
	if !ok || key != certStatusKeyPrefix+"knowhere.live" {
		t.Errorf("certStatusKey(Knowhere.Live) = %q, %v; want the lowercased key", key, ok)
	}
}
