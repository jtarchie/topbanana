package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/egregors/passkey"

	"github.com/jtarchie/topbanana/auth/blob"
)

// The WebAuthn ceremony endpoints are mounted unauthenticated — a caller
// binding their first passkey has no credential to authenticate with — and the
// ceremony itself only asks whether the username resolves to a user, which an
// email address answers. UserStore.Create is therefore the only gate on
// "whose account does this new passkey attach to", and these tests are what
// stop it from silently becoming a pass-through again.

// enrollRig returns an auth instance with probe seeded as an enabled user, plus
// the mux the library's ceremony endpoints are mounted on.
func enrollRig(t *testing.T) (a *Auth, mux *http.ServeMux, probe string) {
	t.Helper()
	probe = "probe+" + freshSuffix() + "@example.com"
	a, err := New(Config{
		Blobs:           blob.NewMemory(),
		Domain:          "localhost",
		SuperAdminEmail: "super+" + freshSuffix() + "@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	err = a.Users.Save(context.Background(), &User{
		Email: probe, Role: RoleSuperAdmin, Created: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed probe user: %v", err)
	}
	mux = http.NewServeMux()
	a.Passkey.MountRoutes(mux, "/auth/")
	return a, mux, probe
}

// registerBegin posts the ceremony's opening request for username, with no
// cookie of any kind — exactly what an anonymous caller can send.
func registerBegin(t *testing.T, mux *http.ServeMux, username string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/passkey/registerBegin",
		strings.NewReader(`{"username":"`+username+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestEnrollment_AnonymousRegisterBeginIsRefused is the regression that
// matters: knowing a user's email address must not be enough to start binding
// your own authenticator to their account. Until the enrollment grant existed,
// this request returned 200 with a challenge, and completing the ceremony
// handed over the account.
func TestEnrollment_AnonymousRegisterBeginIsRefused(t *testing.T) {
	t.Parallel()

	_, mux, probe := enrollRig(t)

	rec := registerBegin(t, mux, probe)
	if rec.Code == http.StatusOK {
		t.Fatalf("registerBegin succeeded for an anonymous caller who only knew the address: %s", rec.Body.String())
	}
	// The refusal must not double as a membership oracle — an unknown address
	// has to be indistinguishable from a known one with no grant.
	unknown := registerBegin(t, mux, "nobody+"+freshSuffix()+"@example.com")
	if unknown.Body.String() != rec.Body.String() {
		t.Errorf("refusal leaks whether the account exists:\n known: %s\n unknown: %s", rec.Body.String(), unknown.Body.String())
	}
}

// TestEnrollment_GrantOpensAndUpdateSpendsIt: the grant is what an authorized
// path (a redeemed invite, or the account page's own session) buys, and it is
// good for one ceremony — Update is the library's only post-registration hook,
// so spending it there is what keeps the unauthenticated window from staying
// open for the rest of the TTL.
func TestEnrollment_GrantOpensAndUpdateSpendsIt(t *testing.T) {
	t.Parallel()

	a, mux, probe := enrollRig(t)
	ctx := context.Background()

	err := a.Users.GrantEnrollment(ctx, probe)
	if err != nil {
		t.Fatalf("GrantEnrollment: %v", err)
	}
	if rec := registerBegin(t, mux, probe); rec.Code != http.StatusOK {
		t.Fatalf("registerBegin after a grant: %d %s", rec.Code, rec.Body.String())
	}

	// Stand in for the library's finishRegistration, which is the only thing
	// that reaches Update on the registration path.
	user, err := a.Users.Load(ctx, probe)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	err = a.Users.Update(passkey.User(user))
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	after, err := a.Users.Load(ctx, probe)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if after.MayEnroll(time.Now()) {
		t.Errorf("grant survived the ceremony; it must be good for one enrollment")
	}
	if rec := registerBegin(t, mux, probe); rec.Code == http.StatusOK {
		t.Errorf("registerBegin succeeded on a spent grant: %s", rec.Body.String())
	}
}

// TestEnrollment_GrantExpires: the window is time-bound, because while it is
// open anyone who knows the address can complete the ceremony.
func TestEnrollment_GrantExpires(t *testing.T) {
	t.Parallel()

	a, mux, probe := enrollRig(t)
	ctx := context.Background()

	user, err := a.Users.Load(ctx, probe)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	user.EnrollUntil = time.Now().UTC().Add(-time.Second)
	err = a.Users.Save(ctx, user)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	if rec := registerBegin(t, mux, probe); rec.Code == http.StatusOK {
		t.Errorf("registerBegin succeeded on an expired grant: %s", rec.Body.String())
	}
}

// TestEnrollment_DisabledUserGetsNoGrant: disabling an account is how an
// operator cuts someone off, so it must also close the door a recovery link
// would otherwise open.
func TestEnrollment_DisabledUserGetsNoGrant(t *testing.T) {
	t.Parallel()

	a, mux, probe := enrollRig(t)
	ctx := context.Background()

	user, err := a.Users.Load(ctx, probe)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	user.Disabled = true
	err = a.Users.Save(ctx, user)
	if err != nil {
		t.Fatalf("save: %v", err)
	}

	err = a.Users.GrantEnrollment(ctx, probe)
	if err == nil {
		t.Error("GrantEnrollment succeeded for a disabled account")
	}
	if rec := registerBegin(t, mux, probe); rec.Code == http.StatusOK {
		t.Errorf("registerBegin succeeded for a disabled account: %s", rec.Body.String())
	}
}
