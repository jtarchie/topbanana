package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/egregors/passkey"
	"github.com/hashicorp/golang-lru/arc/v2"

	"github.com/jtarchie/topbanana/internal/store"
)

// userStorePrefix is the bucket area for user records. One file per user,
// keyed by canonical email. Renames are unsupported (see CLAUDE.md plan):
// disable + invite + transfer if you need a new address.
const userStorePrefix = "_auth/users/"

// userCacheCapacity bounds the ARC cache. Generous because user records are
// small (a few KiB max with a couple of credentials) and the realistic
// upper bound on user count is low tens. Oversizing avoids thrashing in
// edge cases like a super admin paging through the user list.
const userCacheCapacity = 256

// userCacheTTL is the safety net for cache freshness when an invalidation
// is missed (e.g. an out-of-band write from a separate process). Every
// mutation in this package explicitly Remove()s the key before/after the
// S3 write, so the TTL only matters when something bypasses the store.
const userCacheTTL = 60 * time.Second

// ErrUserNotFound is the canonical "no such user" error. Distinct from a
// transport error so callers can branch on it without parsing strings.
var ErrUserNotFound = errors.New("user not found")

type cachedUser struct {
	user     *User
	inserted time.Time
}

// UserStore implements both passkey.UserStore (for the library's ceremony
// handlers) and our own write paths. Backed by S3 via the shared store, with
// an ARC cache so an authenticated request is a single map lookup in
// steady state rather than an S3 GET per call.
//
// Concurrency: each user record is rewritten as a whole document. We
// serialize per-email writes with a striped mutex so two simultaneous
// PutCredential calls on the same user don't clobber each other (the
// passkey library calls PutCredential + Update in sequence on login and
// registration — re-entrant on the same email is rare but possible).
type UserStore struct {
	store *store.Store
	cache *arc.ARCCache[string, cachedUser]

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewUserStore wires the cache and write-locks. Errors only on cache
// construction (size <= 0); panics are unreachable for our constant size.
func NewUserStore(s *store.Store) (*UserStore, error) {
	cache, err := arc.NewARC[string, cachedUser](userCacheCapacity) //nolint:exptostd // arc.NewARC is the API
	if err != nil {
		return nil, fmt.Errorf("auth: build user cache: %w", err)
	}
	return &UserStore{
		store: s,
		cache: cache,
		locks: map[string]*sync.Mutex{},
	}, nil
}

// keyFor produces the absolute bucket key for an email. Always normalized
// so callers can pass raw input.
func userKey(email string) string {
	return userStorePrefix + NormalizeEmail(email) + ".json"
}

// lockFor returns the per-email write mutex. Lazily allocated and never
// evicted: lock objects are small and the user set is bounded.
func (s *UserStore) lockFor(email string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if l, ok := s.locks[email]; ok {
		return l
	}
	l := &sync.Mutex{}
	s.locks[email] = l
	return l
}

// Load reads a user record from S3, populating the cache. Returns
// ErrUserNotFound when the record doesn't exist. Bypasses the cache so it
// can be used to warm the cache or refresh after an external write.
func (s *UserStore) Load(ctx context.Context, email string) (*User, error) {
	email = NormalizeEmail(email)
	obj, err := s.store.ReadRaw(ctx, userKey(email))
	if err != nil {
		return nil, fmt.Errorf("auth: read user %s: %w", email, err)
	}
	if obj.Content == "" {
		return nil, ErrUserNotFound
	}
	user := &User{}
	err = json.Unmarshal([]byte(obj.Content), user)
	if err != nil {
		return nil, fmt.Errorf("auth: parse user %s: %w", email, err)
	}
	s.cache.Add(email, cachedUser{user: user, inserted: time.Now()})
	return user, nil
}

// LookupCached returns the user from the ARC cache, or hits S3 on a miss
// or stale entry. Public so middleware can use it on every authenticated
// request without going through the passkey.User interface.
func (s *UserStore) LookupCached(ctx context.Context, email string) (*User, error) {
	email = NormalizeEmail(email)
	entry, ok := s.cache.Get(email)
	if ok && time.Since(entry.inserted) < userCacheTTL {
		return entry.user, nil
	}
	return s.Load(ctx, email)
}

// Save writes the user record back to S3 and invalidates the cache. Safe
// to call concurrently for different emails; per-email writes serialize.
func (s *UserStore) Save(ctx context.Context, user *User) error {
	email := NormalizeEmail(user.Email)
	if email == "" {
		return errors.New("auth: cannot save user with empty email")
	}
	user.Email = email

	lock := s.lockFor(email)
	lock.Lock()
	defer lock.Unlock()

	body, err := json.Marshal(user)
	if err != nil {
		return fmt.Errorf("auth: marshal user %s: %w", email, err)
	}
	s.cache.Remove(email)
	err = s.store.WriteRaw(ctx, userKey(email), string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("auth: write user %s: %w", email, err)
	}
	s.cache.Add(email, cachedUser{user: user, inserted: time.Now()})
	return nil
}

// List enumerates every user record in the bucket. Used by the super
// admin's /admin/users page. Returns concrete *User values (not the
// passkey.User interface) because callers want Role + Quotas, not just
// the WebAuthn methods.
func (s *UserStore) List(ctx context.Context) ([]*User, error) {
	keys, err := s.store.ListPrefix(ctx, userStorePrefix)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	users := make([]*User, 0, len(keys))
	for _, key := range keys {
		obj, readErr := s.store.ReadRaw(ctx, key)
		if readErr != nil || obj.Content == "" {
			continue
		}
		user := &User{}
		parseErr := json.Unmarshal([]byte(obj.Content), user)
		if parseErr != nil {
			continue
		}
		users = append(users, user)
	}
	return users, nil
}

// Delete drops a user record. Idempotent. The cache is invalidated so a
// re-create reads fresh state.
func (s *UserStore) Delete(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	lock := s.lockFor(email)
	lock.Lock()
	defer lock.Unlock()
	err := s.store.DeleteRaw(ctx, userKey(email))
	if err != nil {
		return fmt.Errorf("auth: delete user %s: %w", email, err)
	}
	s.cache.Remove(email)
	return nil
}

// CreateFromInvite materializes a user record from a validated invite.
// Idempotent for the case where the invite is replayed before consumption
// — returns the existing record without overwriting. Callers MUST validate
// the invite (expiry, UsedBy) before calling.
func (s *UserStore) CreateFromInvite(ctx context.Context, inv Invite) (*User, error) {
	email := NormalizeEmail(inv.Email)
	existing, err := s.Load(ctx, email)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return nil, err
	}
	user := &User{
		Email:   email,
		Role:    inv.Role,
		Quotas:  inv.Quotas,
		Created: time.Now().UTC(),
	}
	err = s.Save(ctx, user)
	if err != nil {
		return nil, err
	}
	slog.Info("auth.user.created", "email", email, "role", string(inv.Role))
	return user, nil
}

// --- passkey.UserStore interface --------------------------------------------

// Create is the library's entry point on the first registerBegin call.
// We deliberately do NOT create new users here — our /register handler
// (commit 3) builds the User record from a validated invite first. So
// Create's only job is to return the existing record, or refuse if the
// caller hasn't gone through the invite flow.
func (s *UserStore) Create(username string) (passkey.User, error) {
	user, err := s.Load(context.Background(), username)
	if err != nil {
		return nil, fmt.Errorf("auth.create: %w", err)
	}
	if user.Disabled {
		return nil, fmt.Errorf("auth.create: user %s is disabled", username)
	}
	return user, nil
}

// Update persists a user record back to S3. Called by the library after
// PutCredential to record a new passkey or an updated sign-count.
func (s *UserStore) Update(u passkey.User) error {
	concrete, ok := u.(*User)
	if !ok {
		return fmt.Errorf("auth.update: unexpected user type %T", u)
	}
	return s.Save(context.Background(), concrete)
}

// Get resolves a user by their WebAuthnID (the email bytes). Used by the
// library after the assertion is verified to fetch the canonical record.
func (s *UserStore) Get(userID []byte) (passkey.User, error) {
	user, err := s.LookupCached(context.Background(), string(userID))
	if err != nil {
		return nil, fmt.Errorf("auth.get: %w", err)
	}
	return user, nil
}

// GetByName resolves a user by their username (also the email). Same path
// as Get but starting from the loginBegin payload's username field.
func (s *UserStore) GetByName(username string) (passkey.User, error) {
	user, err := s.LookupCached(context.Background(), username)
	if err != nil {
		return nil, fmt.Errorf("auth.get_by_name: %w", err)
	}
	return user, nil
}
