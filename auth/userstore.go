package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jtarchie/topbanana/auth/blob"

	"github.com/egregors/passkey"
	"github.com/hashicorp/golang-lru/arc/v2"
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

// EnrollmentGrantTTL is how long an enrollment grant stays live. It spans one
// human interaction — click "add a passkey", then confirm on the device — so
// it is minutes, not hours. During the window, anyone who knows the address
// can complete the ceremony, which is exactly why it closes fast and why
// Update spends it on first use.
const EnrollmentGrantTTL = 10 * time.Minute

// ErrEnrollmentNotAllowed is returned by Create when the account holds no live
// enrollment grant. Callers should not show it to the caller of an
// unauthenticated endpoint — see Create.
var ErrEnrollmentNotAllowed = errors.New("auth: enrollment not allowed")

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
	blobs blob.Blobs
	cache *arc.ARCCache[string, cachedUser]

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// NewUserStore wires the cache and write-locks. Errors only on cache
// construction (size <= 0); panics are unreachable for our constant size.
func NewUserStore(b blob.Blobs) (*UserStore, error) {
	cache, err := arc.NewARC[string, cachedUser](userCacheCapacity) //nolint:exptostd // arc.NewARC is the API
	if err != nil {
		return nil, fmt.Errorf("auth: build user cache: %w", err)
	}
	return &UserStore{
		blobs: b,
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
	obj, err := s.blobs.Get(ctx, userKey(email))
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
	err = s.blobs.Put(ctx, userKey(email), string(body))
	if err != nil {
		return fmt.Errorf("auth: write user %s: %w", email, err)
	}
	s.cache.Add(email, cachedUser{user: user, inserted: time.Now()})
	return nil
}

// List enumerates every user record in the bucket. Used by the super
// admin's /admin/users page. Returns concrete *User values (not the
// passkey.User interface) because callers want Role + Meta, not just
// the WebAuthn methods.
//
// The reads run bounded-parallel because they are one round trip each to a
// remote store and independent of each other: serially, the admin page cost
// one full RTT per account and paid it again on every render. A record that
// fails to read or parse is dropped, same as before — this is a listing, and
// one unreadable object should not blank the page.
func (s *UserStore) List(ctx context.Context) ([]*User, error) {
	keys, err := s.blobs.List(ctx, userStorePrefix)
	if err != nil {
		return nil, fmt.Errorf("auth: list users: %w", err)
	}
	found := make([]*User, len(keys))
	grp, gctx := errgroup.WithContext(ctx)
	grp.SetLimit(listConcurrency)
	for i, key := range keys {
		grp.Go(func() error {
			obj, readErr := s.blobs.Get(gctx, key)
			if readErr != nil || obj.Content == "" {
				return nil
			}
			user := &User{}
			if json.Unmarshal([]byte(obj.Content), user) != nil {
				return nil
			}
			found[i] = user
			return nil
		})
	}
	_ = grp.Wait()

	users := make([]*User, 0, len(found))
	for _, user := range found {
		if user != nil {
			users = append(users, user)
		}
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
	err := s.blobs.Delete(ctx, userKey(email))
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
		Meta:    inv.Meta,
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

// Create is the library's entry point on the first registerBegin call, and the
// only gate on the WebAuthn enrollment ceremony — which is mounted
// unauthenticated, because a caller enrolling their first passkey has no
// credential to authenticate with. Everything the ceremony itself checks, an
// email address satisfies.
//
// So this refuses unless the account holds a live enrollment grant
// (GrantEnrollment). Without that check, knowing an address is enough to bind
// your own authenticator to that account and then sign in as them: /register
// and the account page's "add a passkey" button would be conventions in
// JavaScript, not gates, since nothing stops a client from calling
// registerBegin directly.
//
// It also never creates users. The invite flow is the only thing that
// materializes a record, so an unknown username is a refusal, not a signup.
//
// The three refusals share one error text and log the real reason instead: the
// caller is unauthenticated and the library returns this string to them
// verbatim, so distinguishing "no such user" from "not allowed to enroll"
// hands an anonymous prober a membership oracle for any address.
func (s *UserStore) Create(username string) (passkey.User, error) {
	user, err := s.Load(context.Background(), username)
	if err != nil {
		slog.Warn("auth.enroll.refused", "username", NormalizeEmail(username), "reason", "load", "err", err)
		return nil, ErrEnrollmentNotAllowed
	}
	if user.Disabled {
		slog.Warn("auth.enroll.refused", "email", user.Email, "reason", "disabled")
		return nil, ErrEnrollmentNotAllowed
	}
	if !user.MayEnroll(time.Now()) {
		slog.Warn("auth.enroll.refused", "email", user.Email, "reason", "no_grant")
		return nil, ErrEnrollmentNotAllowed
	}
	return user, nil
}

// GrantEnrollment opens the enrollment window for email, letting the next
// registerBegin/registerFinish pair through Create. Only two callers may issue
// one: the handler that has just validated an invite for this address, and a
// request already carrying this user's own session. Anything else is handing
// out the account.
//
// Loads fresh rather than reusing a cached pointer, so it doesn't mutate a
// *User another in-flight request is reading.
func (s *UserStore) GrantEnrollment(ctx context.Context, email string) error {
	email = NormalizeEmail(email)
	user, err := s.Load(ctx, email)
	if err != nil {
		return err
	}
	if user.Disabled {
		return fmt.Errorf("auth: cannot grant enrollment to disabled user %s", email)
	}
	user.EnrollUntil = time.Now().UTC().Add(EnrollmentGrantTTL)
	err = s.Save(ctx, user)
	if err != nil {
		return err
	}
	slog.Info("auth.enroll.granted", "email", email, "until", user.EnrollUntil)
	return nil
}

// Update persists a user record back to S3. Called by the library after
// PutCredential to record a new passkey or an updated sign-count.
//
// It also SPENDS the enrollment grant, which makes a grant good for one
// ceremony rather than for its whole TTL — the window is unauthenticated, so
// the less of it that stays open after the legitimate enrollment lands, the
// better. This is the only post-ceremony hook the library offers.
//
// The library calls Update on successful login too, so a login racing a
// pending grant spends it and the enrollment then fails at registerBegin.
// That is the safe direction to fail, and the fix is to click the button
// again.
func (s *UserStore) Update(u passkey.User) error {
	concrete, ok := u.(*User)
	if !ok {
		return fmt.Errorf("auth.update: unexpected user type %T", u)
	}
	concrete.EnrollUntil = time.Time{}
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
