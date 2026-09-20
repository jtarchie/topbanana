package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// Passkey recovery is the ordinary invite flow aimed at an account that
// already exists. That only works because redeeming an invite is
// non-destructive to a live record — so these tests drive the real
// /admin/users/:email/recovery → /register handoff rather than asserting on the
// invite object in isolation.

// recoveryRig builds a server plus a signed-in super admin.
func recoveryRig(t *testing.T) (rig *privateTestRig, base string, adminCookie *http.Cookie) {
	t.Helper()
	st := minioStore(t)
	rig = newPrivateRig(t, st, snapshot.New(st, 0))
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)
	adminCookie = rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	return rig, srv.URL, adminCookie
}

// pendingInviteFor finds the one unconsumed invite for email.
func pendingInviteFor(t *testing.T, ctx context.Context, rig *privateTestRig, email string) *auth.Invite {
	t.Helper()
	invites, err := rig.auth.Invites.List(ctx)
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	var found *auth.Invite
	for _, inv := range invites {
		if inv.UsedBy == "" && inv.Email == email {
			if found != nil {
				t.Fatalf("more than one pending invite for %s", email)
			}
			found = inv
		}
	}
	return found
}

// TestAdminUserRecovery_IssuesShortLivedInviteThatKeepsTheAccountIntact: the
// whole feature in one pass. The link is redeemable, it expires on the
// recovery clock rather than the week-long invite clock, and walking the
// /register page with it leaves the existing record — role, quotas, and the
// passkey they still have — exactly as it was.
func TestAdminUserRecovery_IssuesShortLivedInviteThatKeepsTheAccountIntact(t *testing.T) {
	rig, base, adminCookie := recoveryRig(t)
	ctx := context.Background()
	const lost = "lost-device@test"

	err := rig.auth.Users.Save(ctx, &auth.User{
		Email:       lost,
		Role:        auth.RoleAdmin,
		Meta:        []byte(`{"max_apps":7}`),
		Credentials: []webauthn.Credential{{ID: []byte("old-phone")}},
		Created:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	resp := postForm(t, base, "/admin/users/"+lost+"/recovery", adminCookie, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("recovery status: got %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/users?flash=") {
		t.Fatalf("recovery redirect: got %q want /admin/users?flash=...", loc)
	}

	inv := pendingInviteFor(t, ctx, rig, lost)
	if inv == nil {
		t.Fatal("no pending invite was issued")
	}
	// The token is a live credential for a populated account: it must not ride
	// in the redirect, where it lands in history and in the referrer of every
	// asset on the next page.
	if strings.Contains(loc, inv.Token) {
		t.Errorf("recovery token leaked into the redirect URL: %q", loc)
	}
	// Recovery clock, not the 7-day invite clock.
	if ttl := time.Until(inv.Expires); ttl > auth.RecoveryInviteTTL+time.Minute {
		t.Errorf("expires in %s; want at most %s", ttl, auth.RecoveryInviteTTL)
	}
	// The role is copied off the account so the pending-invites table doesn't
	// describe the row wrongly.
	if inv.Role != auth.RoleAdmin {
		t.Errorf("invite role = %q; want the account's own admin", inv.Role)
	}

	// Redeeming: /register materializes nothing new and clobbers nothing. This
	// is the load-bearing assertion — if CreateFromInvite ever started
	// overwriting, recovery would silently wipe quotas and existing passkeys.
	code, _ := authedGetStatus(t, base, "/register?invite="+inv.Token, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /register with a recovery token: got %d want 200", code)
	}
	after, err := rig.auth.Users.Load(ctx, lost)
	if err != nil {
		t.Fatalf("load user after /register: %v", err)
	}
	if after.Role != auth.RoleAdmin {
		t.Errorf("role after /register = %q; want admin", after.Role)
	}
	if string(after.Meta) != `{"max_apps":7}` {
		t.Errorf("meta after /register = %s; want the seeded quotas", after.Meta)
	}
	if len(after.Credentials) != 1 || string(after.Credentials[0].ID) != "old-phone" {
		t.Errorf("credentials after /register = %v; want the existing passkey untouched", after.Credentials)
	}
	// What the redeemed link actually buys. The ceremony endpoints are
	// unauthenticated, so without this the page would render and every
	// registerBegin behind it would be refused.
	if !after.MayEnroll(time.Now()) {
		t.Errorf("redeeming the recovery link left no enrollment grant; the page can render but the passkey can't be bound")
	}
}

// TestAdminUserRecovery_RefusesDisabledAndUnknown: the two cases where a link
// would be worse than no link — one that dies at registerBegin (UserStore.Create
// rejects disabled users), and one for an address with no account, which is an
// invite, not a recovery.
func TestAdminUserRecovery_RefusesDisabledAndUnknown(t *testing.T) {
	rig, base, adminCookie := recoveryRig(t)
	ctx := context.Background()
	const off = "disabled@test"

	err := rig.auth.Users.Save(ctx, &auth.User{
		Email: off, Role: auth.RoleAdmin, Disabled: true, Created: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}

	resp := postForm(t, base, "/admin/users/"+off+"/recovery", adminCookie, nil)
	_ = resp.Body.Close()
	if loc := resp.Header.Get("Location"); !strings.Contains(loc, "error=") {
		t.Errorf("disabled user: got %d %q want a 303 with an error flash", resp.StatusCode, loc)
	}
	if inv := pendingInviteFor(t, ctx, rig, off); inv != nil {
		t.Errorf("issued a link for a disabled account: %s", inv.Token)
	}

	resp = postForm(t, base, "/admin/users/nobody@test/recovery", adminCookie, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown address: got %d want 404", resp.StatusCode)
	}
}

// TestAdminUserRecovery_RefusesNonSuperAdmin: the route mints a credential for
// any account on the instance, so a regular admin must not reach it.
func TestAdminUserRecovery_RefusesNonSuperAdmin(t *testing.T) {
	rig, base, _ := recoveryRig(t)
	ctx := context.Background()
	const target = "victim@test"

	err := rig.auth.Users.Save(ctx, &auth.User{Email: target, Role: auth.RoleAdmin, Created: time.Now().UTC()})
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	plain := rig.session(t, "plain@test", auth.RoleAdmin)

	resp := postForm(t, base, "/admin/users/"+target+"/recovery", plain, url.Values{})
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("a regular admin issued a recovery link for %s", target)
	}
	if inv := pendingInviteFor(t, ctx, rig, target); inv != nil {
		t.Errorf("a regular admin minted a credential for %s: %s", target, inv.Token)
	}
}
