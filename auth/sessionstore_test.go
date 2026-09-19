package auth

import (
	"bytes"
	"context"
	"errors"
	"github.com/jtarchie/topbanana/auth/blob"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/egregors/passkey"
	"github.com/go-webauthn/webauthn/webauthn"
)

// --- memAuthSessionStore (WebAuthn challenges, in-memory) -------------------

func TestMemAuthSessionStore_CreateGetDelete(t *testing.T) {
	t.Parallel()

	store, stop := NewMemAuthSessionStore()
	t.Cleanup(stop)
	data := webauthn.SessionData{
		Challenge: "abc",
		UserID:    []byte("alice@example.com"),
	}

	token, err := store.Create(data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if token == "" {
		t.Fatal("Create returned empty token")
	}

	got, ok := store.Get(token)
	if !ok {
		t.Fatal("Get returned ok=false for fresh token")
	}
	if got.Challenge != "abc" || string(got.UserID) != "alice@example.com" {
		t.Errorf("Get returned %+v", got)
	}

	store.Delete(token)
	if _, ok := store.Get(token); ok {
		t.Errorf("Get after Delete returned ok=true")
	}
}

func TestMemAuthSessionStore_ExpiredEntryEvictedOnGet(t *testing.T) {
	t.Parallel()

	// Reach into the concrete type so we can plant an expired entry without
	// waiting authChallengeTTL (5 minutes) of wall-clock time. The interface
	// returned by NewMemAuthSessionStore wraps the same pointer.
	s := &memAuthSessionStore{data: map[string]memAuthEntry{}}
	token := "stale"
	s.data[token] = memAuthEntry{
		session: webauthn.SessionData{Challenge: "x"},
		expires: time.Now().Add(-time.Second),
	}

	got, ok := s.Get(token)
	if ok || got != nil {
		t.Errorf("Get(expired) = (%+v, %v), want (nil, false)", got, ok)
	}
	if _, stillThere := s.data[token]; stillThere {
		t.Errorf("expired entry was not evicted by Get")
	}
}

func TestMemAuthSessionStore_MissingTokenReturnsFalse(t *testing.T) {
	t.Parallel()

	store, stop := NewMemAuthSessionStore()
	t.Cleanup(stop)
	got, ok := store.Get("no-such-token")
	if ok || got != nil {
		t.Errorf("Get(missing) = (%+v, %v), want (nil, false)", got, ok)
	}
}

func TestMemAuthSessionStore_ConcurrentCreateProducesUniqueTokens(t *testing.T) {
	t.Parallel()

	// Race detector catches concurrent map writes; the test also asserts no
	// duplicate tokens come back across N goroutines.
	store, stop := NewMemAuthSessionStore()
	t.Cleanup(stop)
	const n = 64

	var mu sync.Mutex
	seen := make(map[string]struct{}, n)
	var wg sync.WaitGroup
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := store.Create(webauthn.SessionData{Challenge: "x"})
			if err != nil {
				t.Errorf("Create: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			if _, dup := seen[tok]; dup {
				t.Errorf("duplicate token: %q", tok)
			}
			seen[tok] = struct{}{}
		}()
	}
	wg.Wait()
}

// --- UserSessionStore (S3-backed) -------------------------------------------

func TestUserSessionStore_CreateGetDelete(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	data := passkey.UserSessionData{
		UserID:  []byte("alice+" + freshSuffix() + "@example.com"),
		Expires: time.Now().Add(time.Hour),
	}
	token, err := uss.Create(data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { uss.Delete(token) })

	got, ok := uss.Get(token)
	if !ok || got == nil {
		t.Fatalf("Get returned (%+v, %v)", got, ok)
	}
	if !bytes.Equal(got.UserID, data.UserID) {
		t.Errorf("UserID = %q, want %q", string(got.UserID), string(data.UserID))
	}

	uss.Delete(token)
	if _, ok := uss.Get(token); ok {
		t.Errorf("Get after Delete returned ok=true")
	}
}

func TestUserSessionStore_ExpiredSessionReturnsFalse(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	data := passkey.UserSessionData{
		UserID:  []byte("expired+" + freshSuffix() + "@example.com"),
		Expires: time.Now().Add(-time.Hour),
	}
	token, err := uss.Create(data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { uss.Delete(token) })

	got, ok := uss.Get(token)
	if ok || got != nil {
		t.Errorf("Get(expired) = (%+v, %v), want (nil, false)", got, ok)
	}
}

func TestUserSessionStore_GetReadsThroughCache(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	email := "cache+" + freshSuffix() + "@example.com"
	data := passkey.UserSessionData{UserID: []byte(email), Expires: time.Now().Add(time.Hour)}
	token, err := uss.Create(data)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { uss.Delete(token) })

	// Drop the cache entry so the next Get must read from S3, exercising
	// the .load() path that's otherwise skipped on freshly-created sessions.
	uss.cache.Remove(token)
	got, ok := uss.Get(token)
	if !ok || got == nil {
		t.Fatalf("Get after cache eviction failed to load from store: ok=%v", ok)
	}
	if string(got.UserID) != email {
		t.Errorf("loaded UserID = %q, want %q", string(got.UserID), email)
	}
}

func TestUserSessionStore_RevokeAllForUserOnlyDropsMatching(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	suffix := freshSuffix()
	victimEmail := "victim+" + suffix + "@example.com"
	survivorEmail := "survivor+" + suffix + "@example.com"

	victim1, err := uss.Create(passkey.UserSessionData{UserID: []byte(victimEmail), Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("create victim1: %v", err)
	}
	victim2, err := uss.Create(passkey.UserSessionData{UserID: []byte(victimEmail), Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("create victim2: %v", err)
	}
	survivor, err := uss.Create(passkey.UserSessionData{UserID: []byte(survivorEmail), Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("create survivor: %v", err)
	}
	t.Cleanup(func() {
		uss.Delete(victim1)
		uss.Delete(victim2)
		uss.Delete(survivor)
	})

	err = uss.RevokeAllForUser(context.Background(), victimEmail)
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}

	// Deliberately do NOT evict the victims from the cache here: RevokeAllForUser
	// is required to evict them itself, so a buggy implementation that only
	// deletes the S3 object would leave a warm cache entry and fail below.
	// The survivor IS evicted manually so its lookup hits S3 — that proves the
	// revoke didn't wrongly delete a non-matching record (cache could otherwise
	// mask a bad S3 delete).
	uss.cache.Remove(survivor)

	if _, ok := uss.Get(victim1); ok {
		t.Errorf("victim1 still present after revoke")
	}
	if _, ok := uss.Get(victim2); ok {
		t.Errorf("victim2 still present after revoke")
	}
	if _, ok := uss.Get(survivor); !ok {
		t.Errorf("survivor was wrongly revoked")
	}
}

// TestUserSessionStore_RevokeAllEvictsCacheImmediately is the security-critical
// case behind "sign out everywhere": after a revoke, a session that was warm in
// the LRU must stop validating *immediately*, without waiting out sessionCacheTTL
// and without any external cache eviction. A regression here means a revoked
// device keeps its access for up to a minute.
func TestUserSessionStore_RevokeAllEvictsCacheImmediately(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	email := "warm+" + freshSuffix() + "@example.com"
	token, err := uss.Create(passkey.UserSessionData{UserID: []byte(email), Expires: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { uss.Delete(token) })

	// Warm the cache the way a real authenticated request would.
	if _, ok := uss.Get(token); !ok {
		t.Fatalf("session should be valid before revoke")
	}

	err = uss.RevokeAllForUser(context.Background(), email)
	if err != nil {
		t.Fatalf("RevokeAllForUser: %v", err)
	}

	// No manual cache.Remove, no TTL sleep: the revoke must have evicted it.
	if _, ok := uss.Get(token); ok {
		t.Errorf("session still validates from cache immediately after revoke")
	}
}

func TestUserSessionStore_GetUnknownTokenReturnsFalse(t *testing.T) {
	t.Parallel()

	st := blob.NewMemory()
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	if _, ok := uss.Get("nope-" + freshSuffix()); ok {
		t.Errorf("Get(unknown) returned ok=true")
	}
}

// downBlobs fails every read while down is set, the way a bucket that is
// briefly unreachable does.
type downBlobs struct {
	blob.Blobs
	down atomic.Bool
}

func (b *downBlobs) Get(ctx context.Context, key string) (blob.Object, error) {
	if b.down.Load() {
		return blob.Object{}, errors.New("bucket unreachable")
	}
	return b.Blobs.Get(ctx, key) //nolint:wrapcheck // transparent wrapper
}

// A store that cannot answer is not a store that answered no. Get has no way
// to say so, which is why Lookup exists: knowhere logged people out — deleting
// the session — whenever a read failed.
func TestUserSessionStore_LookupKeepsFaultsApartFromMisses(t *testing.T) {
	t.Parallel()

	st := &downBlobs{Blobs: blob.NewMemory()}
	writer, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}
	token, err := writer.Create(passkey.UserSessionData{
		UserID:  []byte("alice+" + freshSuffix() + "@example.com"),
		Expires: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// A second store over the same bucket has a cold cache, so the read
	// reaches the bucket — what the 60s TTL produces in production.
	cold, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	st.down.Store(true)
	data, err := cold.Lookup(context.Background(), token)
	if err == nil || errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("Lookup with the bucket down = (%+v, %v), want a fault, not a verdict", data, err)
	}
	if _, ok := cold.Get(token); ok {
		t.Errorf("Get with the bucket down returned ok=true")
	}

	st.down.Store(false)
	data, err = cold.Lookup(context.Background(), token)
	if err != nil || data == nil {
		t.Fatalf("Lookup once the bucket is back = (%+v, %v), want the session", data, err)
	}

	missing, err := cold.Lookup(context.Background(), "no-such-token")
	if missing != nil || !errors.Is(err, ErrSessionNotFound) {
		t.Errorf("Lookup of an unknown token = (%+v, %v), want ErrSessionNotFound", missing, err)
	}
}
