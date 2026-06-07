package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/egregors/passkey"
	"github.com/go-webauthn/webauthn/webauthn"
	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/jtarchie/topbanana/internal/store"
)

// authChallengeTTL bounds how long a WebAuthn challenge is honoured. Five
// minutes matches the egregors/passkey default for the auth-session
// cookie, so the in-memory record can't outlive the matching cookie.
const authChallengeTTL = 5 * time.Minute

// sessionStorePrefix holds persistent user sessions (the post-login cookie
// state). One file per session token. Deleted on logout or expiry.
const sessionStorePrefix = "_auth/sessions/"

// sessionCacheCapacity caps the in-memory LRU for user-session reads.
// Authenticated requests do one lookup per call, so this needs to cover
// every concurrently active session; 1k accommodates well past any
// realistic active set for this tool.
const sessionCacheCapacity = 1024

// sessionCacheTTL keeps the cache from serving a deleted session for more
// than one polling cycle of any frontend. Logout invalidates immediately;
// the TTL only matters for cross-process eviction (not currently a thing).
const sessionCacheTTL = 60 * time.Second

// newToken returns a 32-byte URL-safe random string. Used for both auth
// challenges and persistent session IDs. Library only requires uniqueness
// and unguessability; 256 bits is plenty.
func newToken() (string, error) {
	buf := make([]byte, 32)
	_, err := rand.Read(buf)
	if err != nil {
		return "", fmt.Errorf("auth: rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// --- Auth session store (WebAuthn challenges) -------------------------------

// memAuthSessionStore keeps short-lived WebAuthn challenge state in memory.
// Challenges are 5-minute, single-use, and lose nothing meaningful on a
// process restart — the user just retries.
type memAuthSessionStore struct {
	mu        sync.Mutex
	data      map[string]memAuthEntry
	done      chan struct{}
	closeOnce sync.Once
}

type memAuthEntry struct {
	session webauthn.SessionData
	expires time.Time
}

// NewMemAuthSessionStore returns a SessionStore[webauthn.SessionData] that
// holds entries in a map with TTL eviction, plus a stop function that
// terminates the background sweep. Background sweep runs every half-TTL so
// a stalled registration doesn't pile up forever. The stop function is
// idempotent — production calls it on shutdown; tests defer it to satisfy
// goleak.
func NewMemAuthSessionStore() (passkey.SessionStore[webauthn.SessionData], func()) {
	s := &memAuthSessionStore{
		data: map[string]memAuthEntry{},
		done: make(chan struct{}),
	}
	go s.sweep(authChallengeTTL / 2)
	return s, s.close
}

// close stops the sweep goroutine. Safe to call more than once.
func (s *memAuthSessionStore) close() {
	s.closeOnce.Do(func() { close(s.done) })
}

func (s *memAuthSessionStore) Create(data webauthn.SessionData) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[token] = memAuthEntry{session: data, expires: time.Now().Add(authChallengeTTL)}
	return token, nil
}

func (s *memAuthSessionStore) Delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, token)
}

func (s *memAuthSessionStore) Get(token string) (*webauthn.SessionData, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.data[token]
	if !ok {
		return nil, false
	}
	if time.Now().After(entry.expires) {
		delete(s.data, token)
		return nil, false
	}
	return &entry.session, true
}

func (s *memAuthSessionStore) sweep(interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			now := time.Now()
			s.mu.Lock()
			for k, v := range s.data {
				if now.After(v.expires) {
					delete(s.data, k)
				}
			}
			s.mu.Unlock()
		}
	}
}

// --- User session store (post-login cookie state) ---------------------------

// UserSessionStore persists logged-in sessions to S3 so they survive
// restarts and so a super admin can revoke them out-of-band by deleting
// the underlying object. An LRU cache absorbs the per-request lookup hot
// path.
type UserSessionStore struct {
	store *store.Store
	cache *lru.Cache[string, cachedSession]
}

type cachedSession struct {
	data     passkey.UserSessionData
	inserted time.Time
}

// NewUserSessionStore wires the persistent store + read cache. Returns
// passkey.SessionStore so it can be plugged into passkey.Config directly.
func NewUserSessionStore(s *store.Store) (*UserSessionStore, error) {
	cache, err := lru.New[string, cachedSession](sessionCacheCapacity)
	if err != nil {
		return nil, fmt.Errorf("auth: build session cache: %w", err)
	}
	return &UserSessionStore{store: s, cache: cache}, nil
}

func sessionKey(token string) string {
	return sessionStorePrefix + token + ".json"
}

func (s *UserSessionStore) Create(data passkey.UserSessionData) (string, error) {
	token, err := newToken()
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(data)
	if err != nil {
		return "", fmt.Errorf("auth: marshal session: %w", err)
	}
	err = s.store.WriteRaw(context.Background(), sessionKey(token), string(body), "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("auth: write session: %w", err)
	}
	s.cache.Add(token, cachedSession{data: data, inserted: time.Now()})
	return token, nil
}

func (s *UserSessionStore) Delete(token string) {
	s.cache.Remove(token)
	err := s.store.DeleteRaw(context.Background(), sessionKey(token))
	if err != nil {
		// Sessions don't expose an error path on the interface; log and move
		// on. A stale S3 object is harmless because the next Get will see
		// the missing cache entry, re-read it, find it, then we're back to
		// where we started — at worst one extra GET per stale token.
		return
	}
}

func (s *UserSessionStore) Get(token string) (*passkey.UserSessionData, bool) {
	entry, ok := s.cache.Get(token)
	if ok && time.Since(entry.inserted) < sessionCacheTTL {
		// Treat expired (past UserSessionData.Expires) like a miss so the
		// library refuses the cookie and we drop the entry.
		if time.Now().After(entry.data.Expires) {
			s.cache.Remove(token)
			return nil, false
		}
		return &entry.data, true
	}
	loaded, ok := s.load(token)
	if !ok {
		return nil, false
	}
	return &loaded, true
}

func (s *UserSessionStore) load(token string) (passkey.UserSessionData, bool) {
	obj, err := s.store.ReadRaw(context.Background(), sessionKey(token))
	if err != nil {
		return passkey.UserSessionData{}, false
	}
	if obj.Content == "" {
		return passkey.UserSessionData{}, false
	}
	var data passkey.UserSessionData
	err = json.Unmarshal([]byte(obj.Content), &data)
	if err != nil {
		return passkey.UserSessionData{}, false
	}
	if time.Now().After(data.Expires) {
		// Tidy up an expired session record so a future revoke-all doesn't
		// have to wade through them.
		_ = s.store.DeleteRaw(context.Background(), sessionKey(token))
		return passkey.UserSessionData{}, false
	}
	s.cache.Add(token, cachedSession{data: data, inserted: time.Now()})
	return data, true
}

// RevokeAllForUser deletes every persisted session whose UserID matches the
// given email. Used by the super admin "revoke sessions" action. O(N) over
// the session list, which is fine at our scale.
func (s *UserSessionStore) RevokeAllForUser(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	keys, err := s.store.ListPrefix(ctx, sessionStorePrefix)
	if err != nil {
		return fmt.Errorf("auth: list sessions: %w", err)
	}
	var firstErr error
	for _, key := range keys {
		obj, err := s.store.ReadRaw(ctx, key)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if obj.Content == "" {
			continue
		}
		var data passkey.UserSessionData
		err = json.Unmarshal([]byte(obj.Content), &data)
		if err != nil {
			continue
		}
		if string(data.UserID) != email {
			continue
		}
		err = s.store.DeleteRaw(ctx, key)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		// Evict the in-memory LRU too, otherwise a just-revoked session keeps
		// validating from cache for up to sessionCacheTTL (60s). Best-effort:
		// drop the cache entry even if the S3 delete above errored — a stale
		// cache entry (session still honoured) is worse than a stale S3 object.
		token := strings.TrimSuffix(strings.TrimPrefix(key, sessionStorePrefix), ".json")
		s.cache.Remove(token)
	}
	if firstErr != nil {
		return fmt.Errorf("auth: revoke sessions: %w", firstErr)
	}
	return nil
}

// Static interface assertions so a type-signature drift in the library
// fails at build time, not at runtime when the first login happens.
var (
	_ passkey.SessionStore[webauthn.SessionData]    = (*memAuthSessionStore)(nil)
	_ passkey.SessionStore[passkey.UserSessionData] = (*UserSessionStore)(nil)
)
