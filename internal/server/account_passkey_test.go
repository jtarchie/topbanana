package server_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// seedUserWithCredentials writes a user record carrying the given credential
// ids so the passkey-removal path has real credentials to operate on (the
// test session injector creates a credential-less user).
func seedUserWithCredentials(t *testing.T, ctx context.Context, rig *privateTestRig, email string, ids ...string) {
	t.Helper()
	creds := make([]webauthn.Credential, 0, len(ids))
	for _, id := range ids {
		creds = append(creds, webauthn.Credential{ID: []byte(id)})
	}
	err := rig.auth.Users.Save(ctx, &auth.User{
		Email:       email,
		Role:        auth.RoleAdmin,
		Credentials: creds,
		Created:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("seed user %s: %v", email, err)
	}
}

// credForm builds the remove-passkey form body for a credential id.
func credForm(id string) url.Values {
	return url.Values{"id": {base64.RawURLEncoding.EncodeToString([]byte(id))}}
}

// TestAccountRemovePasskey_DropsOneKeepsRest: with two passkeys, removing one
// leaves exactly the other bound.
func TestAccountRemovePasskey_DropsOneKeepsRest(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const email = "twokeys@test"
	seedUserWithCredentials(t, ctx, rig, email, "cred-keep", "cred-drop")
	cookie := rig.session(t, email, auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/account/passkeys/delete", cookie, credForm("cred-drop"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/account?flash=") {
		t.Errorf("remove redirect: got %q want /account?flash=...", loc)
	}

	user, err := rig.auth.Users.Load(ctx, email)
	if err != nil {
		t.Fatalf("load after remove: %v", err)
	}
	if len(user.Credentials) != 1 {
		t.Fatalf("creds after remove: got %d want 1", len(user.Credentials))
	}
	if string(user.Credentials[0].ID) != "cred-keep" {
		t.Errorf("wrong credential survived: %q", user.Credentials[0].ID)
	}
}

// TestAccountRemovePasskey_RefusesLast: removing the only passkey is refused so
// the user can't lock themselves out (passkey-only auth, no password reset).
func TestAccountRemovePasskey_RefusesLast(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	rig := newPrivateRig(t, st, snapSvc)
	srv := httptest.NewServer(rig.handler)
	t.Cleanup(srv.Close)

	const email = "onekey@test"
	seedUserWithCredentials(t, ctx, rig, email, "only-cred")
	cookie := rig.session(t, email, auth.RoleAdmin)

	resp := postForm(t, srv.URL, "/account/passkeys/delete", cookie, credForm("only-cred"))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("refuse status: got %d want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/account?error=") {
		t.Errorf("refuse redirect: got %q want /account?error=...", loc)
	}

	user, err := rig.auth.Users.Load(ctx, email)
	if err != nil {
		t.Fatalf("load after refused remove: %v", err)
	}
	if len(user.Credentials) != 1 {
		t.Errorf("last passkey was removed despite the guard: %d creds", len(user.Credentials))
	}
}
