package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jtarchie/bloomhollow/internal/store"
)

const inviteStorePrefix = "_auth/invites/"

// DefaultInviteTTL is how long a non-bootstrap invite stays redeemable.
// One week balances "easy to send and use later" against "stops a stale
// token from being burnable months later if the inbox is breached."
const DefaultInviteTTL = 7 * 24 * time.Hour

// BootstrapInviteTTL is the shorter window for the first super-admin
// invite that the server logs on startup. The operator should consume it
// promptly; if they don't, it regenerates on the next restart.
const BootstrapInviteTTL = 24 * time.Hour

// ErrInviteNotFound is returned when a token has no backing record. Also
// returned for consumed invites — callers should treat both as "no" with
// no further differentiation, since exposing the difference would leak
// information about prior recipients.
var ErrInviteNotFound = errors.New("invite not found")

// ErrInviteExpired is returned when an invite is past its Expires time but
// hasn't yet been consumed. Kept distinct so the UI can render a "expired"
// message rather than a generic 404.
var ErrInviteExpired = errors.New("invite expired")

// InviteStore is the S3-backed lifecycle for one-time invite tokens.
type InviteStore struct {
	store *store.Store
}

func NewInviteStore(s *store.Store) *InviteStore {
	return &InviteStore{store: s}
}

func inviteKey(token string) string {
	return inviteStorePrefix + token + ".json"
}

// Issue generates a fresh invite for the given email + role + quotas.
// Returns the token so the caller can build the /register?invite=<token>
// URL. Doesn't dedupe — issuing two invites to the same email lets the
// operator hand out a new one without revoking the old.
func (s *InviteStore) Issue(ctx context.Context, email string, role Role, quotas Quotas, ttl time.Duration) (*Invite, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv := &Invite{
		Token:   token,
		Email:   NormalizeEmail(email),
		Role:    role,
		Quotas:  quotas,
		Created: now,
		Expires: now.Add(ttl),
	}
	err = s.save(ctx, inv)
	if err != nil {
		return nil, err
	}
	return inv, nil
}

// IssueOrReuseBootstrap returns the existing unconsumed bootstrap invite
// for an email if there is one, otherwise issues a fresh one. Used so the
// startup log doesn't churn through new tokens on every restart while the
// super admin is figuring out their passkey setup.
func (s *InviteStore) IssueOrReuseBootstrap(ctx context.Context, email string) (*Invite, error) {
	email = NormalizeEmail(email)
	existing, ok, err := s.findUnconsumedFor(ctx, email)
	if err != nil {
		return nil, err
	}
	if ok {
		return existing, nil
	}
	return s.Issue(ctx, email, RoleSuperAdmin, Quotas{}, BootstrapInviteTTL)
}

// Get reads an invite by token. Returns ErrInviteNotFound for missing or
// consumed records, ErrInviteExpired for past-expiry records. Callers
// should not treat the difference as security-meaningful.
func (s *InviteStore) Get(ctx context.Context, token string) (*Invite, error) {
	obj, err := s.store.ReadRaw(ctx, inviteKey(token))
	if err != nil {
		return nil, fmt.Errorf("auth: read invite: %w", err)
	}
	if obj.Content == "" {
		return nil, ErrInviteNotFound
	}
	inv := &Invite{}
	err = json.Unmarshal([]byte(obj.Content), inv)
	if err != nil {
		return nil, fmt.Errorf("auth: parse invite: %w", err)
	}
	if inv.UsedBy != "" {
		return nil, ErrInviteNotFound
	}
	if time.Now().After(inv.Expires) {
		return nil, ErrInviteExpired
	}
	return inv, nil
}

// Consume marks an invite as used by the given email. Idempotent for the
// case where a previous attempt failed mid-flow (UsedBy already set by the
// same email is treated as success). Caller is expected to have verified
// the invite via Get before calling.
func (s *InviteStore) Consume(ctx context.Context, token, consumer string) error {
	consumer = NormalizeEmail(consumer)
	obj, err := s.store.ReadRaw(ctx, inviteKey(token))
	if err != nil {
		return fmt.Errorf("auth: read invite: %w", err)
	}
	if obj.Content == "" {
		return ErrInviteNotFound
	}
	inv := &Invite{}
	err = json.Unmarshal([]byte(obj.Content), inv)
	if err != nil {
		return fmt.Errorf("auth: parse invite: %w", err)
	}
	if inv.UsedBy != "" && inv.UsedBy != consumer {
		return ErrInviteNotFound
	}
	inv.UsedBy = consumer
	return s.save(ctx, inv)
}

// Revoke deletes an invite outright. Used by super admin to invalidate an
// invite sent to a wrong address before the recipient has bound a passkey.
func (s *InviteStore) Revoke(ctx context.Context, token string) error {
	err := s.store.DeleteRaw(ctx, inviteKey(token))
	if err != nil {
		return fmt.Errorf("auth: revoke invite: %w", err)
	}
	return nil
}

// List returns every invite record under the prefix, used by the super
// admin UI to render the pending-invite table. O(N) over invite count;
// fine at our scale.
func (s *InviteStore) List(ctx context.Context) ([]*Invite, error) {
	keys, err := s.store.ListPrefix(ctx, inviteStorePrefix)
	if err != nil {
		return nil, fmt.Errorf("auth: list invites: %w", err)
	}
	invites := make([]*Invite, 0, len(keys))
	for _, key := range keys {
		obj, err := s.store.ReadRaw(ctx, key)
		if err != nil || obj.Content == "" {
			continue
		}
		inv := &Invite{}
		err = json.Unmarshal([]byte(obj.Content), inv)
		if err != nil {
			continue
		}
		invites = append(invites, inv)
	}
	return invites, nil
}

func (s *InviteStore) findUnconsumedFor(ctx context.Context, email string) (*Invite, bool, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, false, err
	}
	now := time.Now()
	for _, inv := range all {
		if inv.UsedBy == "" && inv.Email == email && now.Before(inv.Expires) {
			return inv, true, nil
		}
	}
	return nil, false, nil
}

func (s *InviteStore) save(ctx context.Context, inv *Invite) error {
	body, err := json.Marshal(inv)
	if err != nil {
		return fmt.Errorf("auth: marshal invite: %w", err)
	}
	err = s.store.WriteRaw(ctx, inviteKey(inv.Token), string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("auth: write invite: %w", err)
	}
	return nil
}
