package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/internal/quotas"
)

// The invite tools are the first MCP surface gated on the caller's *role*
// rather than on site ownership, so these drive the real bearer → user lookup →
// role check path. The refusal case matters most: the tools appear in every
// client's tool list, so a regular admin discovering them must get nothing.

// seedSuperAdmin creates a super-admin user record in the shared auth store.
func seedSuperAdmin(t *testing.T, blobs blob.Blobs, email string) {
	t.Helper()
	a := testAuth(t, blobs)
	_, err := a.InjectTestSession(context.Background(), email, auth.RoleSuperAdmin)
	if err != nil {
		t.Fatalf("seed super admin %s: %v", email, err)
	}
}

// testAuth builds an auth.Auth over the blobs the server shares, for tests that
// need to assert on identity records the tools wrote.
func testAuth(t *testing.T, blobs blob.Blobs) *auth.Auth {
	t.Helper()
	a, err := auth.New(auth.Config{
		Blobs:           blobs,
		Domain:          "localhost",
		SuperAdminEmail: "super@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

// mustCallTool runs a tool, fails on a tool-level error, and decodes the JSON
// result. Complements callTool (mcp_write_verify_e2e_test.go), which returns
// the raw result for tests that assert on failures.
func mustCallTool(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res := callTool(t, session, name, args)
	text := toolText(res)
	if res.IsError {
		t.Fatalf("%s errored: %s", name, text)
	}
	out := map[string]any{}
	err := json.Unmarshal([]byte(text), &out)
	if err != nil {
		t.Fatalf("%s result is not JSON (%v): %s", name, err, text)
	}
	return out
}

func TestMCP_IssueInvite_SuperAdminRoundTrip(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const admin = "boss@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, admin)
	seedSuperAdmin(t, authBlobs, admin)
	session := connectMCP(t, srv, admin)

	out := mustCallTool(t, session, "issue_invite", map[string]any{
		"email": "Newbie@Example.COM", "role": "admin",
		"max_apps": 3, "models": map[string]any{"author": "anthropic/claude-opus-4-7"},
	})
	invite, ok := out["invite"].(map[string]any)
	if !ok {
		t.Fatalf("no invite object in result: %v", out)
	}
	token, _ := invite["token"].(string)
	if token == "" {
		t.Fatalf("no token in invite: %v", invite)
	}
	// The email is normalized on the way in, so the invite binds to the
	// canonical address the register flow will compare against.
	if got, _ := invite["email"].(string); got != "newbie@example.com" {
		t.Errorf("email = %q; want newbie@example.com", got)
	}
	// The URL is what the operator actually hands over — it must be absolute
	// and carry the token, or it's not redeemable.
	url, _ := invite["url"].(string)
	if !strings.HasSuffix(url, "/register?invite="+token) || !strings.HasPrefix(url, "http") {
		t.Errorf("url = %q; want an absolute /register?invite=<token> link", url)
	}

	// The token resolves through the real store, with the quotas the tool
	// encoded — the capability /admin/users' form does not have.
	stored, err := testAuth(t, authBlobs).Invites.Get(ctx, token)
	if err != nil {
		t.Fatalf("issued invite is not redeemable: %v", err)
	}
	if stored.Role != auth.RoleAdmin {
		t.Errorf("role = %q; want admin", stored.Role)
	}
	q := quotas.OfInvite(stored)
	if q.MaxApps != 3 {
		t.Errorf("max_apps = %d; want 3", q.MaxApps)
	}
	if q.AllowedModels["author"] != "anthropic/claude-opus-4-7" {
		t.Errorf("author model = %q; want anthropic/claude-opus-4-7", q.AllowedModels["author"])
	}

	// list_invites surfaces it while it's still pending.
	listed := mustCallTool(t, session, "list_invites", nil)
	body, _ := json.Marshal(listed)
	if !strings.Contains(string(body), token) {
		t.Errorf("list_invites missing the pending invite: %s", body)
	}

	// revoke_invite makes the URL stop working.
	mustCallTool(t, session, "revoke_invite", map[string]any{"token": token})
	_, err = testAuth(t, authBlobs).Invites.Get(ctx, token)
	if err == nil {
		t.Fatal("revoked invite is still redeemable")
	}

	// Revoking again — or revoking a typo — must not report success. A blob
	// delete is idempotent, so without an existence check the operator would
	// be told a still-live link was killed.
	res := callTool(t, session, "revoke_invite", map[string]any{"token": token})
	if !res.IsError || !strings.Contains(toolText(res), "no pending invite") {
		t.Errorf("revoking a nonexistent token reported success: %s", toolText(res))
	}
}

// TestMCP_IssueInvite_ClampsAbsurdTTL: ttl_hours is multiplied into a
// time.Duration, so a huge value must be clamped before the multiply — an
// overflow wraps negative and mints an invite that is already expired.
func TestMCP_IssueInvite_ClampsAbsurdTTL(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	const admin = "boss@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, freshSlug(t), admin)
	seedSuperAdmin(t, authBlobs, admin)
	session := connectMCP(t, srv, admin)

	out := mustCallTool(t, session, "issue_invite", map[string]any{
		"email": "late@example.com", "ttl_hours": 9000000000000,
	})
	invite, _ := out["invite"].(map[string]any)
	token, _ := invite["token"].(string)

	stored, err := testAuth(t, authBlobs).Invites.Get(ctx, token)
	if err != nil {
		t.Fatalf("invite with an absurd ttl is not redeemable: %v", err)
	}
	if !stored.Expires.After(time.Now()) {
		t.Errorf("expiry is in the past: %s", stored.Expires)
	}
	// 30 days is the ceiling; allow an hour of slack for clock/rounding.
	if stored.Expires.After(time.Now().Add(31 * 24 * time.Hour)) {
		t.Errorf("expiry %s exceeds the 30-day cap", stored.Expires)
	}
}

// TestMCP_InviteTools_RefuseNonSuperAdmin is the point of the role gate: a
// regular admin holding a perfectly valid bearer token gets nothing from any of
// the three tools.
func TestMCP_InviteTools_RefuseNonSuperAdmin(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner) // RoleAdmin
	session := connectMCP(t, srv, owner)

	cases := map[string]map[string]any{
		"issue_invite":  {"email": "someone@example.com"},
		"list_invites":  nil,
		"revoke_invite": {"token": "whatever"},
	}
	for name, args := range cases {
		res := callTool(t, session, name, args)
		text := toolText(res)
		if !res.IsError {
			t.Errorf("%s succeeded for a non-super-admin: %s", name, text)
			continue
		}
		if !strings.Contains(text, "super-admin") {
			t.Errorf("%s error should name the missing role, got: %s", name, text)
		}
	}

	// Nothing was issued along the way.
	invites, err := testAuth(t, authBlobs).Invites.List(context.Background())
	if err != nil {
		t.Fatalf("list invites: %v", err)
	}
	for _, inv := range invites {
		if inv.Email == "someone@example.com" {
			t.Fatal("a refused issue_invite still wrote an invite")
		}
	}
}

// TestMCP_IssueInvite_RejectsBadRole pins the deliberate divergence from
// templates.Get-style fallbacks: a bad role is an error, not a silent
// downgrade to admin.
func TestMCP_IssueInvite_RejectsBadRole(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const admin = "boss@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, admin)
	seedSuperAdmin(t, authBlobs, admin)
	session := connectMCP(t, srv, admin)

	res := callTool(t, session, "issue_invite", map[string]any{
		"email": "x@example.com", "role": "superadmin",
	})
	text := toolText(res)
	if !res.IsError {
		t.Fatalf("bad role accepted: %s", text)
	}
	if !strings.Contains(text, "invalid role") {
		t.Errorf("unexpected error text: %s", text)
	}
}
