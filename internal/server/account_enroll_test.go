package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestAccountEnroll_GrantsOnlyYourself: the account page's "add a passkey"
// button needs an enrollment grant, and the route that issues one must key off
// the session rather than anything in the request — otherwise it is the same
// hole the grant closes, reachable by any signed-in user.
func TestAccountEnroll_GrantsOnlyYourself(t *testing.T) {
	st := minioStore(t)
	rig := newPrivateRig(t, st, snapshot.New(st, 0))
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)
	base := srv.URL
	ctx := context.Background()
	const mine, theirs = "mine@test", "theirs@test"

	for _, email := range []string{mine, theirs} {
		err := rig.auth.Users.Save(ctx, &auth.User{Email: email, Role: auth.RoleAdmin, Created: time.Now().UTC()})
		if err != nil {
			t.Fatalf("seed %s: %v", email, err)
		}
	}

	resp := postForm(t, base, "/account/passkeys/enroll", rig.session(t, mine, auth.RoleAdmin),
		url.Values{"email": {theirs}, "username": {theirs}})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("enroll status: got %d want 204", resp.StatusCode)
	}

	self, err := rig.auth.Users.Load(ctx, mine)
	if err != nil {
		t.Fatalf("load self: %v", err)
	}
	if !self.MayEnroll(time.Now()) {
		t.Errorf("no grant for the signed-in user")
	}
	other, err := rig.auth.Users.Load(ctx, theirs)
	if err != nil {
		t.Fatalf("load other: %v", err)
	}
	if other.MayEnroll(time.Now()) {
		t.Errorf("form fields steered the grant onto %s — it must come from the session", theirs)
	}

	// Unauthenticated, the route is just a redirect to the login page.
	resp = postForm(t, base, "/account/passkeys/enroll", nil, nil)
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		t.Errorf("an anonymous caller opened an enrollment window")
	}
}
