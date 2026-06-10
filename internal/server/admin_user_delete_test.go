package server_test

import (
	"context"
	"encoding/json"
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

// metaOwner reads the persisted SiteMeta.OwnerID for a slug.
func metaOwner(t *testing.T, ctx context.Context, rig *privateTestRig, slug string) string {
	t.Helper()
	obj, err := rig.store.Read(ctx, slug, build.MetaFile)
	if err != nil {
		t.Fatalf("read meta %s: %v", slug, err)
	}
	var meta build.SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &meta)
	if err != nil {
		t.Fatalf("unmarshal meta %s: %v", slug, err)
	}
	return meta.OwnerID
}

// TestAdminUserDelete_CascadeForTarget: a super admin deletes a regular user,
// taking their sites and sessions with them, while another user's site (and the
// admin's own) survive.
func TestAdminUserDelete_CascadeForTarget(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const bob = "bob-del@test"
	base := freshSlug(t)
	bobSite, adminSite := base+"bob", base+"adm"
	seedSite(t, ctx, rig, snapSvc, bobSite, bob)
	seedSite(t, ctx, rig, snapSvc, adminSite, testAdminUser)

	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	bobCookie := rig.session(t, bob, auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/admin/users/"+bob, adminCookie, url.Values{"_method": {"DELETE"}, "confirm": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/users?flash=") {
		t.Errorf("delete redirect: got %q want /admin/users?flash=...", loc)
	}

	// Bob's site is gone; the admin's untouched.
	if files, _ := st.List(ctx, bobSite); len(files) != 0 {
		t.Errorf("bob's site not deleted: %v", files)
	}
	if got := mustRead(t, ctx, st, adminSite, "index.html"); got == "" {
		t.Errorf("admin's own site was wrongly deleted")
	}
	// Bob's record + session are gone.
	_, err := rig.auth.Users.Load(ctx, bob)
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("bob record: got %v want ErrUserNotFound", err)
	}
	if code, loc := authedGetStatus(t, srv.URL, "/account", bobCookie); code != http.StatusSeeOther || !strings.HasPrefix(loc, "/login") {
		t.Errorf("bob session post-delete: got %d %q want 303 -> /login", code, loc)
	}
}

// TestAdminUserDelete_TransferThenDelete: deleting with a transfer_to keeps the
// user's sites alive under the new owner instead of destroying them.
func TestAdminUserDelete_TransferThenDelete(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const dave = "dave@test"
	const carol = "carol-rcpt@test"
	site := freshSlug(t) + "xfer"
	seedSite(t, ctx, rig, snapSvc, site, dave)

	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	_ = rig.session(t, dave, auth.RoleAdmin)  // the user being deleted
	_ = rig.session(t, carol, auth.RoleAdmin) // recipient must exist + be enabled

	resp := postForm(t, srv.URL, "/admin/users/"+dave, adminCookie,
		url.Values{"_method": {"DELETE"}, "confirm": {dave}, "disposition": {"transfer"}, "transfer_to": {carol}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("transfer-delete status: got %d want 303", resp.StatusCode)
	}

	// Site survives, now owned by carol; dave's record is gone.
	if got := mustRead(t, ctx, st, site, "index.html"); got == "" {
		t.Errorf("transferred site was deleted")
	}
	if owner := metaOwner(t, ctx, rig, site); owner != carol {
		t.Errorf("site owner after transfer: got %q want %q", owner, carol)
	}
	_, err := rig.auth.Users.Load(ctx, dave)
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("dave record: got %v want ErrUserNotFound", err)
	}
}

// TestAdminUserDelete_RefusesSelf: a super admin can't delete their own account
// from the admin table — that path lives on /account.
func TestAdminUserDelete_RefusesSelf(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)

	resp := postForm(t, srv.URL, "/admin/users/"+testAdminUser, adminCookie, url.Values{"_method": {"DELETE"}, "confirm": {testAdminUser}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("self-delete status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/users?error=") {
		t.Errorf("self-delete redirect: got %q want /admin/users?error=...", loc)
	}
	_, err := rig.auth.Users.Load(ctx, testAdminUser)
	if err != nil {
		t.Errorf("admin record must survive a refused self-delete: %v", err)
	}
}

// TestAdminUserDelete_AnotherSuperAdmin: a super admin may remove a second
// super admin as long as one remains (the caller).
func TestAdminUserDelete_AnotherSuperAdmin(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const boss2 = "boss2@test"
	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	_ = rig.session(t, boss2, auth.RoleSuperAdmin) // second operator

	resp := postForm(t, srv.URL, "/admin/users/"+boss2, adminCookie, url.Values{"_method": {"DELETE"}, "confirm": {boss2}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete super admin status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/users?flash=") {
		t.Errorf("delete redirect: got %q want flash", loc)
	}
	_, err := rig.auth.Users.Load(ctx, boss2)
	if !errors.Is(err, auth.ErrUserNotFound) {
		t.Errorf("boss2 record: got %v want ErrUserNotFound", err)
	}
	_, err = rig.auth.Users.Load(ctx, testAdminUser)
	if err != nil {
		t.Errorf("caller super admin must survive: %v", err)
	}
}

// TestAdminUserDelete_UnknownEmail404: a matching-confirm delete of a
// nonexistent user is a 404, not a redirect.
func TestAdminUserDelete_UnknownEmail404(t *testing.T) {
	st := minioStore(t)
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const ghost = "ghost@test"
	adminCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)

	resp := postForm(t, srv.URL, "/admin/users/"+ghost, adminCookie, url.Values{"_method": {"DELETE"}, "confirm": {ghost}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown-email status: got %d want 404", resp.StatusCode)
	}
}

// TestAdminUserDelete_RequiresSuperAdmin: a regular admin can't reach the route
// at all — requireSuperAdmin answers 404 so the route's existence stays hidden.
func TestAdminUserDelete_RequiresSuperAdmin(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const victim = "victim-admin@test"
	_ = rig.session(t, victim, auth.RoleAdmin)
	regularCookie := rig.session(t, "regular@test", auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/admin/users/"+victim, regularCookie, url.Values{"_method": {"DELETE"}, "confirm": {victim}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("regular-admin status: got %d want 404", resp.StatusCode)
	}
	_, err := rig.auth.Users.Load(ctx, victim)
	if err != nil {
		t.Errorf("victim must survive an unauthorized delete attempt: %v", err)
	}
}
