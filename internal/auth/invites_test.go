package auth

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestInviteStore_IssueAndGetRoundtrip(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}

	is := NewInviteStore(st)
	ctx := context.Background()
	email := "alice+" + freshSuffix() + "@example.com"

	inv, err := is.Issue(ctx, email, RoleAdmin, Quotas{MaxApps: 5}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	if inv.Token == "" {
		t.Errorf("Issue returned empty token")
	}
	if inv.Email != NormalizeEmail(email) {
		t.Errorf("Email = %q, want normalized", inv.Email)
	}
	if inv.Role != RoleAdmin {
		t.Errorf("Role = %q, want admin", inv.Role)
	}
	if inv.Quotas.MaxApps != 5 {
		t.Errorf("Quotas not persisted: %+v", inv.Quotas)
	}
	if inv.Expires.Before(time.Now()) {
		t.Errorf("Expires already past: %s", inv.Expires)
	}

	got, err := is.Get(ctx, inv.Token)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Token != inv.Token {
		t.Errorf("Get returned token %q, want %q", got.Token, inv.Token)
	}
}

func TestInviteStore_GetUnknownTokenReturnsNotFound(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)

	_, err := is.Get(context.Background(), "no-such-token-"+freshSuffix())
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("err = %v, want ErrInviteNotFound", err)
	}
}

func TestInviteStore_GetExpiredReturnsExpired(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	inv, err := is.Issue(ctx, "exp+"+freshSuffix()+"@example.com", RoleAdmin, Quotas{}, time.Millisecond)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	// Issue with a 1ms TTL — by the time we Get it the expiry has passed.
	time.Sleep(20 * time.Millisecond)
	_, err = is.Get(ctx, inv.Token)
	if !errors.Is(err, ErrInviteExpired) {
		t.Errorf("err = %v, want ErrInviteExpired", err)
	}
}

func TestInviteStore_ConsumeMarksUsedAndGetReturnsNotFound(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	email := "consume+" + freshSuffix() + "@example.com"
	inv, err := is.Issue(ctx, email, RoleAdmin, Quotas{}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	err = is.Consume(ctx, inv.Token, email)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	// Once consumed, Get must report NotFound (consumed records are
	// indistinguishable from missing ones at the API surface, per the
	// invites.go comment).
	_, err = is.Get(ctx, inv.Token)
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("Get after Consume = %v, want ErrInviteNotFound", err)
	}
}

func TestInviteStore_ConsumeTwiceBySameConsumerIsIdempotent(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	email := "twice+" + freshSuffix() + "@example.com"
	inv, err := is.Issue(ctx, email, RoleAdmin, Quotas{}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	err = is.Consume(ctx, inv.Token, email)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	err = is.Consume(ctx, inv.Token, email)
	if err != nil {
		t.Errorf("second consume by same email should be a noop, got: %v", err)
	}
}

func TestInviteStore_ConsumeByDifferentEmailRejected(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	suffix := freshSuffix()
	first := "first+" + suffix + "@example.com"
	second := "second+" + suffix + "@example.com"
	inv, err := is.Issue(ctx, first, RoleAdmin, Quotas{}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	err = is.Consume(ctx, inv.Token, first)
	if err != nil {
		t.Fatalf("first consume: %v", err)
	}
	err = is.Consume(ctx, inv.Token, second)
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("Consume by different email = %v, want ErrInviteNotFound", err)
	}
}

func TestInviteStore_RevokeDeletesRecord(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	inv, err := is.Issue(ctx, "revoke+"+freshSuffix()+"@example.com", RoleAdmin, Quotas{}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	err = is.Revoke(ctx, inv.Token)
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	_, err = is.Get(ctx, inv.Token)
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("Get after Revoke = %v, want ErrInviteNotFound", err)
	}
}

func TestInviteStore_ListIncludesIssued(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	inv, err := is.Issue(ctx, "listed+"+freshSuffix()+"@example.com", RoleAdmin, Quotas{}, DefaultInviteTTL)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, inv.Token) })

	list, err := is.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var found bool
	for _, candidate := range list {
		if candidate.Token == inv.Token {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("List did not include token %q", inv.Token)
	}
}

func TestInviteStore_IssueOrReuseBootstrap_ReusesUnconsumed(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	email := "boot+" + freshSuffix() + "@example.com"
	first, err := is.IssueOrReuseBootstrap(ctx, email)
	if err != nil {
		t.Fatalf("first IssueOrReuseBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, first.Token) })

	second, err := is.IssueOrReuseBootstrap(ctx, email)
	if err != nil {
		t.Fatalf("second IssueOrReuseBootstrap: %v", err)
	}
	if first.Token != second.Token {
		t.Errorf("second call minted a new token (%q != %q); should reuse", second.Token, first.Token)
	}
	if second.Role != RoleSuperAdmin {
		t.Errorf("bootstrap invite Role = %q, want super_admin", second.Role)
	}
}

func TestInviteStore_IssueOrReuseBootstrap_IssuesAfterConsume(t *testing.T) {
	t.Parallel()

	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run invite store tests")
	}
	is := NewInviteStore(st)
	ctx := context.Background()

	email := "boot2+" + freshSuffix() + "@example.com"
	first, err := is.IssueOrReuseBootstrap(ctx, email)
	if err != nil {
		t.Fatalf("first IssueOrReuseBootstrap: %v", err)
	}
	err = is.Consume(ctx, first.Token, email)
	if err != nil {
		t.Fatalf("Consume: %v", err)
	}

	second, err := is.IssueOrReuseBootstrap(ctx, email)
	if err != nil {
		t.Fatalf("second IssueOrReuseBootstrap: %v", err)
	}
	t.Cleanup(func() { _ = is.Revoke(ctx, second.Token) })
	if second.Token == first.Token {
		t.Errorf("consumed bootstrap was reused")
	}
}

func TestInvite_JSONRoundtrip(t *testing.T) {
	t.Parallel()

	// Pure marshal/unmarshal sanity — runs without MinIO so we catch type
	// regressions in the JSON tags even when the integration suite is skipped.
	inv := Invite{
		Token:   "tok",
		Email:   "x@example.com",
		Role:    RoleAdmin,
		Quotas:  Quotas{MaxApps: 3},
		Created: time.Now().UTC().Truncate(time.Second),
		Expires: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
	}
	body, err := json.Marshal(inv)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Invite
	err = json.Unmarshal(body, &got)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Token != inv.Token || got.Email != inv.Email || got.Role != inv.Role || got.Quotas.MaxApps != inv.Quotas.MaxApps {
		t.Errorf("roundtrip mismatch: got=%+v want=%+v", got, inv)
	}
}

func TestNewToken_ProducesUniqueOpaqueValues(t *testing.T) {
	t.Parallel()

	seen := map[string]struct{}{}
	for i := range 256 {
		tok, err := newToken()
		if err != nil {
			t.Fatalf("newToken: %v", err)
		}
		if len(tok) < 32 {
			t.Errorf("token too short: %q", tok)
		}
		if _, dup := seen[tok]; dup {
			t.Fatalf("duplicate token after %d iterations: %q", i, tok)
		}
		seen[tok] = struct{}{}
	}
}
