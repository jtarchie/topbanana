package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// collabRig seeds one private site owned by owner, brings the server up (so
// the startup index rebuild sees the sidecar), and returns the rig plus its
// URL. Private on purpose: the subdomain gate is one of the five access checks
// collaboration has to reach, and it's the only one visible without a cookie.
func collabRig(t *testing.T, slug, owner string) (*privateTestRig, string) {
	t.Helper()
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>shared</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: owner, Private: true})

	rig := newPrivateRig(t, st, snapSvc)
	httpSrv := httptest.NewServer(rig.handler)
	t.Cleanup(httpSrv.Close)
	return rig, httpSrv.URL
}

// readMetaDirect reads the persisted sidecar, bypassing the server — the point
// of most of these assertions is that the grant survives in storage, not just
// in the in-memory index.
func readMetaDirect(t *testing.T, rig *privateTestRig, slug string) build.SiteMeta {
	t.Helper()
	obj, err := rig.store.Read(context.Background(), slug, build.MetaFile)
	if err != nil {
		t.Fatalf("read meta %s: %v", slug, err)
	}
	var meta build.SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &meta)
	if err != nil {
		t.Fatalf("unmarshal meta %s: %v", slug, err)
	}
	return meta
}

// siteGet fetches the site's own subdomain — the private-content gate, which
// reads the session cookie directly rather than going through requireUser.
func siteGet(t *testing.T, base, slug string, cookie *http.Cookie) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/", nil)
	if err != nil {
		t.Fatalf("new site GET: %v", err)
	}
	req.Host = slug + ".localhost"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("site GET: %v", err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestCollaborator_GrantAndRevoke walks the whole lifecycle against the real
// HTTP surface: a stranger is invisible, a share grants both the admin page
// and the private site, and removing it takes both away again.
func TestCollaborator_GrantAndRevoke(t *testing.T) {
	const owner = "collab-owner@test"
	const bob = "collab-bob@test"
	slug := "collab-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	bobCookie := rig.session(t, bob, auth.RoleAdmin)

	// Before the share: 404 on the admin page (deliberately not 403 — a
	// stranger must not learn the slug exists) and 404 on the private site.
	if status, _ := authedGetStatus(t, base, "/manage/"+slug, bobCookie); status != http.StatusNotFound {
		t.Fatalf("pre-share /manage: got %d want 404", status)
	}
	if status := siteGet(t, base, slug, bobCookie); status != http.StatusNotFound {
		t.Fatalf("pre-share private site: got %d want 404", status)
	}

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("share: got %d want 303", resp.StatusCode)
	}

	if status, _ := authedGetStatus(t, base, "/manage/"+slug, bobCookie); status != http.StatusOK {
		t.Errorf("shared /manage: got %d want 200", status)
	}
	if status := siteGet(t, base, slug, bobCookie); status != http.StatusOK {
		t.Errorf("shared private site: got %d want 200", status)
	}

	// The grant is persisted, not just indexed — a restart has to keep it.
	meta := readMetaDirect(t, rig, slug)
	if len(meta.Collaborators) != 1 || meta.Collaborators[0] != bob {
		t.Errorf("sidecar collaborators: got %v want [%s]", meta.Collaborators, bob)
	}

	resp = postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"_method": {"DELETE"}, "collaborator_email": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unshare: got %d want 303", resp.StatusCode)
	}

	if status, _ := authedGetStatus(t, base, "/manage/"+slug, bobCookie); status != http.StatusNotFound {
		t.Errorf("post-revoke /manage: got %d want 404", status)
	}
	if status := siteGet(t, base, slug, bobCookie); status != http.StatusNotFound {
		t.Errorf("post-revoke private site: got %d want 404", status)
	}
}

// TestCollaborator_CannotDeleteTransferOrShare is the containment test: a
// collaborator has full working access, and none of the three owner-only
// actions. 403 rather than 404 here — they can already see the site.
func TestCollaborator_CannotDeleteTransferOrShare(t *testing.T) {
	const owner = "own2@test"
	const bob = "bob2@test"
	const carol = "carol2@test"
	slug := "collab-guard-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	bobCookie := rig.session(t, bob, auth.RoleAdmin)
	_ = rig.session(t, carol, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()

	cases := []struct {
		name string
		path string
		form url.Values
	}{
		{"delete", "/apps/" + slug, url.Values{"_method": {"DELETE"}, "confirm": {slug}}},
		{"transfer", "/apps/" + slug + "/transfer", url.Values{"new_owner_email": {carol}}},
		{"share", "/apps/" + slug + "/collaborators", url.Values{"collaborator_email": {carol}}},
		{"unshare", "/apps/" + slug + "/collaborators", url.Values{"_method": {"DELETE"}, "collaborator_email": {bob}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := postForm(t, base, tc.path, bobCookie, tc.form)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusForbidden {
				t.Errorf("got %d want 403", r.StatusCode)
			}
		})
	}

	// The site is still there and still owned by the same person.
	meta := readMetaDirect(t, rig, slug)
	if meta.OwnerID != owner {
		t.Errorf("owner changed to %q", meta.OwnerID)
	}
	if len(meta.Collaborators) != 1 {
		t.Errorf("collaborators changed: %v", meta.Collaborators)
	}
}

// TestCollaborator_UnknownEmailRejected pins the "existing users only" policy:
// sharing must never mint an account, so an address nobody has registered is a
// 404 rather than an invite.
func TestCollaborator_UnknownEmailRejected(t *testing.T) {
	const owner = "own3@test"
	slug := "collab-unknown-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)
	ownerCookie := rig.session(t, owner, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {"ghost@test"}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown email: got %d want 404", resp.StatusCode)
	}
	if meta := readMetaDirect(t, rig, slug); len(meta.Collaborators) != 0 {
		t.Errorf("collaborators written for an unknown email: %v", meta.Collaborators)
	}
}

// TestCollaborator_AccountDeletionRevokesShares is the one that bites if
// skipped: deleting an account has to strip the address from other people's
// sidecars, or re-registering it silently restores access nobody re-granted.
func TestCollaborator_AccountDeletionRevokesShares(t *testing.T) {
	const owner = "own4@test"
	const bob = "bob4@test"
	slug := "collab-del-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	bobCookie := rig.session(t, bob, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()

	resp = postForm(t, base, "/account/delete", bobCookie,
		url.Values{"confirm": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("account delete: got %d want 303", resp.StatusCode)
	}

	meta := readMetaDirect(t, rig, slug)
	if len(meta.Collaborators) != 0 {
		t.Errorf("deleted account still listed: %v", meta.Collaborators)
	}
	if meta.OwnerID != owner {
		t.Errorf("owner lost their site: %q", meta.OwnerID)
	}
	// The collaborator's deletion must not have cascaded into the owner's site.
	_, err := rig.store.Read(context.Background(), slug, "index.html")
	if err != nil {
		t.Errorf("owner's site was deleted: %v", err)
	}

	// Re-registering the same address must not restore access.
	newBobCookie := rig.session(t, bob, auth.RoleAdmin)
	if status, _ := authedGetStatus(t, base, "/manage/"+slug, newBobCookie); status != http.StatusNotFound {
		t.Errorf("re-registered address regained access: got %d want 404", status)
	}
}

// TestCollaborator_TransferDropsRecipientFromList keeps the two lists from
// overlapping: the new owner must not also sit in Collaborators, where
// removing them would look like revoking an owner.
func TestCollaborator_TransferDropsRecipientFromList(t *testing.T) {
	const owner = "own5@test"
	const bob = "bob5@test"
	slug := "collab-xfer-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	_ = rig.session(t, bob, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()

	resp = postForm(t, base, "/apps/"+slug+"/transfer", ownerCookie,
		url.Values{"new_owner_email": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("transfer: got %d want 303", resp.StatusCode)
	}

	meta := readMetaDirect(t, rig, slug)
	if meta.OwnerID != bob {
		t.Fatalf("owner: got %q want %q", meta.OwnerID, bob)
	}
	if len(meta.Collaborators) != 0 {
		t.Errorf("new owner still listed as collaborator: %v", meta.Collaborators)
	}
}

// TestCollaborator_DisabledAccountLosesPrivateAccess covers the operator's
// kill switch on the one path that doesn't run through requireUser: the
// private-site proxy reads the session cookie directly, and session revocation
// on disable is explicitly best-effort, so a warm cookie has to be re-checked
// against the current record.
func TestCollaborator_DisabledAccountLosesPrivateAccess(t *testing.T) {
	const owner = "own6@test"
	const bob = "bob6@test"
	slug := "collab-disabled-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	bobCookie := rig.session(t, bob, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()
	if status := siteGet(t, base, slug, bobCookie); status != http.StatusOK {
		t.Fatalf("shared private site before disable: got %d want 200", status)
	}

	ctx := context.Background()
	user, err := rig.auth.Users.Load(ctx, bob)
	if err != nil {
		t.Fatalf("load %s: %v", bob, err)
	}
	user.Disabled = true
	err = rig.auth.Users.Save(ctx, user)
	if err != nil {
		t.Fatalf("disable %s: %v", bob, err)
	}

	// Same cookie, same grant — the account is what changed.
	if status := siteGet(t, base, slug, bobCookie); status != http.StatusNotFound {
		t.Errorf("disabled collaborator still reads the private site: got %d want 404", status)
	}
	if status, _ := authedGetStatus(t, base, "/manage/"+slug, bobCookie); status == http.StatusOK {
		t.Error("disabled collaborator still reaches /manage")
	}
}

// TestCollaborator_ReassignOnUserDeleteDropsDuplicate guards the sidecar
// invariant on the other ownership-moving path: deleting a user with
// transfer_to must not leave the new owner sitting in their own collaborator
// list, where the Remove button next to their address reads as revoking an
// owner.
func TestCollaborator_ReassignOnUserDeleteDropsDuplicate(t *testing.T) {
	const owner = "own7@test"
	const bob = "bob7@test"
	slug := "collab-reassign-" + freshSlug(t)
	rig, base := collabRig(t, slug, owner)

	superCookie := rig.session(t, testAdminUser, auth.RoleSuperAdmin)
	ownerCookie := rig.session(t, owner, auth.RoleAdmin)
	_ = rig.session(t, bob, auth.RoleAdmin)

	resp := postForm(t, base, "/apps/"+slug+"/collaborators", ownerCookie,
		url.Values{"collaborator_email": {bob}})
	_ = resp.Body.Close()

	resp = postForm(t, base, "/admin/users/"+owner, superCookie,
		url.Values{"_method": {"DELETE"}, "confirm": {owner}, "disposition": {"transfer"}, "transfer_to": {bob}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete-with-transfer: got %d want 303", resp.StatusCode)
	}

	meta := readMetaDirect(t, rig, slug)
	if meta.OwnerID != bob {
		t.Fatalf("owner = %q, want %q", meta.OwnerID, bob)
	}
	if len(meta.Collaborators) != 0 {
		t.Errorf("new owner still listed as a collaborator: %v", meta.Collaborators)
	}
}
