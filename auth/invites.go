package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/jtarchie/topbanana/auth/blob"
)

// listConcurrency bounds the parallel reads a store listing issues. The
// listings here are one small JSON object per record, so the limit is about
// not opening an unbounded number of connections to the object store on a
// bucket that has grown, not about the records being expensive.
const listConcurrency = 16

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
	blobs blob.Blobs
}

func NewInviteStore(b blob.Blobs) *InviteStore {
	return &InviteStore{blobs: b}
}

func inviteKey(token string) string {
	return inviteStorePrefix + token + ".json"
}

// Issue generates a fresh invite for the given email + role + quotas.
// Returns the token so the caller can build the /register?invite=<token>
// URL. Doesn't dedupe — issuing two invites to the same email lets the
// operator hand out a new one without revoking the old.
func (s *InviteStore) Issue(ctx context.Context, email string, role Role, meta json.RawMessage, ttl time.Duration) (*Invite, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	inv := &Invite{
		Token:   token,
		Email:   NormalizeEmail(email),
		Role:    role,
		Meta:    meta,
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
	return s.Issue(ctx, email, RoleSuperAdmin, nil, BootstrapInviteTTL)
}

// Get reads an invite by token. Returns ErrInviteNotFound for missing or
// consumed records, ErrInviteExpired for past-expiry records. Callers
// should not treat the difference as security-meaningful.
func (s *InviteStore) Get(ctx context.Context, token string) (*Invite, error) {
	obj, err := s.blobs.Get(ctx, inviteKey(token))
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
	obj, err := s.blobs.Get(ctx, inviteKey(token))
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
	err := s.blobs.Delete(ctx, inviteKey(token))
	if err != nil {
		return fmt.Errorf("auth: revoke invite: %w", err)
	}
	return nil
}

// List returns every invite record under the prefix, used by the super
// admin UI to render the pending-invite table.
//
// Reads run bounded-parallel: each is an independent round trip, and the
// count only grows — consumed and expired invites stay in the bucket, so the
// admin page was paying a serial RTT for records it then filtered out.
func (s *InviteStore) List(ctx context.Context) ([]*Invite, error) {
	keys, err := s.blobs.List(ctx, inviteStorePrefix)
	if err != nil {
		return nil, fmt.Errorf("auth: list invites: %w", err)
	}
	found := make([]*Invite, len(keys))
	grp, gctx := errgroup.WithContext(ctx)
	grp.SetLimit(listConcurrency)
	for i, key := range keys {
		grp.Go(func() error {
			obj, readErr := s.blobs.Get(gctx, key)
			if readErr != nil || obj.Content == "" {
				return nil
			}
			inv := &Invite{}
			if json.Unmarshal([]byte(obj.Content), inv) != nil {
				return nil
			}
			found[i] = inv
			return nil
		})
	}
	_ = grp.Wait()

	invites := make([]*Invite, 0, len(found))
	for _, inv := range found {
		if inv != nil {
			invites = append(invites, inv)
		}
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
	err = s.blobs.Put(ctx, inviteKey(inv.Token), string(body))
	if err != nil {
		return fmt.Errorf("auth: write invite: %w", err)
	}
	return nil
}
