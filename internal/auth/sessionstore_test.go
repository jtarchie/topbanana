package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/egregors/passkey"
	"github.com/go-webauthn/webauthn/webauthn"
)

// --- memAuthSessionStore (WebAuthn challenges, in-memory) -------------------

func TestMemAuthSessionStore_CreateGetDelete(t *testing.T) {
	t.Parallel()

	store := NewMemAuthSessionStore()
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

	store := NewMemAuthSessionStore()
	got, ok := store.Get("no-such-token")
	if ok || got != nil {
		t.Errorf("Get(missing) = (%+v, %v), want (nil, false)", got, ok)
	}
}

func TestMemAuthSessionStore_ConcurrentCreateProducesUniqueTokens(t *testing.T) {
	t.Parallel()

	// Race detector catches concurrent map writes; the test also asserts no
	// duplicate tokens come back across N goroutines.
	store := NewMemAuthSessionStore()
	const n = 64

	var mu sync.Mutex
	seen := make(map[string]struct{}, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
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

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run session store tests")
	}
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
	if string(got.UserID) != string(data.UserID) {
		t.Errorf("UserID = %q, want %q", string(got.UserID), string(data.UserID))
	}

	uss.Delete(token)
	if _, ok := uss.Get(token); ok {
		t.Errorf("Get after Delete returned ok=true")
	}
}

func TestUserSessionStore_ExpiredSessionReturnsFalse(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run session store tests")
	}
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

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run session store tests")
	}
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

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run session store tests")
	}
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

	// Drop the cache so the survivor lookup hits S3, otherwise a buggy
	// RevokeAllForUser that wrongly deletes S3 records would still pass via
	// the in-memory cache.
	uss.cache.Remove(victim1)
	uss.cache.Remove(victim2)
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

func TestUserSessionStore_GetUnknownTokenReturnsFalse(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run session store tests")
	}
	uss, err := NewUserSessionStore(st)
	if err != nil {
		t.Fatalf("NewUserSessionStore: %v", err)
	}

	if _, ok := uss.Get("nope-" + freshSuffix()); ok {
		t.Errorf("Get(unknown) returned ok=true")
	}
}
