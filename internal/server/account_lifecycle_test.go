package server_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// noRedirectClient surfaces 3xx responses instead of following them so a test
// can assert the redirect target.
func noRedirectClient() *http.Client {
	return &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

// authedGetStatus probes whether a cookie still authenticates against the main
// (localhost) host: requireUser answers 200 when the session is valid and 303
// → /login once it's been revoked. Returns (status, Location).
func authedGetStatus(t *testing.T, base, path string, cookie *http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatalf("new GET %s: %v", path, err)
	}
	req.Host = "localhost"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("Location")
}

// postForm issues a form POST against the main (localhost) host with the cookie,
// without following the redirect.
func postForm(t *testing.T, base, path string, cookie *http.Cookie, form url.Values) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST %s: %v", path, err)
	}
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// seedSite drops an index.html + an owned meta sidecar so the cascade has
// something to find and delete.
func seedSite(t *testing.T, ctx context.Context, rig *privateTestRig, snapSvc *snapshot.Service, slug, owner string) {
	t.Helper()
	cleanupSlug(t, ctx, rig.store, snapSvc, slug)
	mustWrite(t, ctx, rig.store, slug, "index.html", "<h1>"+slug+"</h1>", "text/html")
	writeMeta(t, ctx, rig.store, slug, build.SiteMeta{Template: "blank", OwnerID: owner})
}

// TestAccountSignOutEverywhere_RevokesEveryDevice mints two sessions for the
// same user (two devices), signs out everywhere from one, and asserts BOTH
// stop authenticating — immediately, proving the cross-device revoke plus the
// session-cache eviction land together.
func TestAccountSignOutEverywhere_RevokesEveryDevice(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const email = "multi-device@test"
	laptop := rig.session(t, email, auth.RoleAdmin)
	phone := rig.session(t, email, auth.RoleAdmin)

	// Both devices authenticate before the revoke.
	if code, _ := authedGetStatus(t, srv.URL, "/account", laptop); code != http.StatusOK {
		t.Fatalf("laptop pre-revoke: got %d want 200", code)
	}
	if code, _ := authedGetStatus(t, srv.URL, "/account", phone); code != http.StatusOK {
		t.Fatalf("phone pre-revoke: got %d want 200", code)
	}

	resp := postForm(t, srv.URL, "/account/sign-out-everywhere", laptop, url.Values{})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-out status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("sign-out redirect: got %q want /login", loc)
	}

	// The device that triggered it AND the other device are both dead now.
	if code, loc := authedGetStatus(t, srv.URL, "/account", laptop); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Errorf("laptop post-revoke: got %d %q, want 303 -> /login", code, loc)
	}
	if code, loc := authedGetStatus(t, srv.URL, "/account", phone); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Errorf("phone post-revoke: got %d %q, want 303 -> /login", code, loc)
	}
}

// TestAccountDelete_CascadesSitesSessionsUser is the core scenario: a regular
// user deletes their account; their own sites and sessions disappear, the user
// record is gone, and a site owned by someone else is untouched.
func TestAccountDelete_CascadesSitesSessionsUser(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const alice = "alice@test"
	const bob = "bob@test"
	base := freshSlug(t)
	aliceSite1, aliceSite2, bobSite := base+"a", base+"b", base+"c"
	seedSite(t, ctx, rig, snapSvc, aliceSite1, alice)
	seedSite(t, ctx, rig, snapSvc, aliceSite2, alice)
	seedSite(t, ctx, rig, snapSvc, bobSite, bob)

	aliceCookie := rig.session(t, alice, auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/account/delete", aliceCookie, url.Values{"confirm": {alice}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
		t.Errorf("delete redirect: got %q, want /login...", loc)
	}

	// Alice's sites are gone; Bob's is intact.
	for _, slug := range []string{aliceSite1, aliceSite2} {
		files, err := st.List(ctx, slug)
		if err != nil {
			t.Fatalf("list %s: %v", slug, err)
		}
		if len(files) != 0 {
			t.Errorf("alice site %s not emptied: %v", slug, files)
		}
	}
	if got := mustRead(t, ctx, st, bobSite, "index.html"); got == "" {
		t.Errorf("bob's site was wrongly deleted")
	}

	// The user record is gone and the session no longer authenticates.
	_, err := rig.auth.Users.Load(ctx, alice)
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("alice record: got err %v, want ErrUserNotFound", err)
	}
	if code, loc := authedGetStatus(t, srv.URL, "/account", aliceCookie); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Errorf("alice session post-delete: got %d %q, want 303 -> /login", code, loc)
	}
}

// TestAccountDelete_WrongConfirmRejected: a mismatched confirmation is a 400
// and leaves the account and its sites fully intact.
func TestAccountDelete_WrongConfirmRejected(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const carol = "carol@test"
	slug := freshSlug(t) + "keep"
	seedSite(t, ctx, rig, snapSvc, slug, carol)
	carolCookie := rig.session(t, carol, auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/account/delete", carolCookie, url.Values{"confirm": {"not-my-email"}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong-confirm status: got %d want 400", resp.StatusCode)
	}
	_, err := rig.auth.Users.Load(ctx, carol)
	if err != nil {
		t.Errorf("carol record should survive a rejected delete: %v", err)
	}
	if got := mustRead(t, ctx, st, slug, "index.html"); got == "" {
		t.Errorf("carol's site was deleted despite a rejected confirmation")
	}
}

// TestAccountDelete_SuperAdminRefused: an operator can't self-delete from
// /account — the platform must never be left adminless. The request bounces to
// /account with an error and the record stays put.
func TestAccountDelete_SuperAdminRefused(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)

	resp := postForm(t, srv.URL, "/account/delete", adminCookie, url.Values{"confirm": {testAdminUser}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("super-admin delete status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/account?error=") {
		t.Errorf("super-admin delete redirect: got %q, want /account?error=...", loc)
	}
	_, err := rig.auth.Users.Load(ctx, testAdminUser)
	if err != nil {
		t.Errorf("super-admin record must survive a refused self-delete: %v", err)
	}
}
