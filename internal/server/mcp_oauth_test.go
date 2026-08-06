package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/storetest"
)

func TestVerifyPKCE(t *testing.T) {
	verifier := "the-quick-brown-fox-jumps-over-the-lazy-dog-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !verifyPKCE(verifier, challenge) {
		t.Fatal("matching verifier/challenge should pass")
	}
	if verifyPKCE("wrong-verifier", challenge) {
		t.Fatal("mismatched verifier should fail")
	}
	if verifyPKCE("", challenge) {
		t.Fatal("empty verifier should fail")
	}
	if verifyPKCE(verifier, "") {
		t.Fatal("empty challenge should fail")
	}
}

// mustNewCode issues a code or fails the test — the error path is exercised
// on its own in the handler tests.
func mustNewCode(t *testing.T, st *mcpOAuthState, ctx context.Context) string {
	t.Helper()
	code, err := st.newCode(ctx, "user@example.com", "client-1", "https://cb", "challenge")
	if err != nil {
		t.Fatalf("newCode: %v", err)
	}
	return code
}

func TestMCPOAuthState_CodeSingleUse(t *testing.T) {
	ctx := context.Background()
	st := newMCPOAuthState(storetest.New(t, 0))
	code := mustNewCode(t, st, ctx)

	ac, ok := st.takeCode(ctx, code)
	if !ok {
		t.Fatal("first takeCode should succeed")
	}
	if ac.Email != "user@example.com" || ac.ClientID != "client-1" || ac.RedirectURI != "https://cb" {
		t.Fatalf("code payload mismatch: %+v", ac)
	}
	if _, ok := st.takeCode(ctx, code); ok {
		t.Fatal("second takeCode should fail (single use)")
	}
}

// TestMCPOAuthState_CodeRedeemableAcrossInstances is the multi-instance case:
// /oauth/authorize lands on one instance and the /oauth/token POST is
// load-balanced to another, which has never seen the code in memory. Two
// states over one store stand in for the two processes.
func TestMCPOAuthState_CodeRedeemableAcrossInstances(t *testing.T) {
	ctx := context.Background()
	backing := storetest.New(t, 0)
	issuer := newMCPOAuthState(backing)
	redeemer := newMCPOAuthState(backing)

	code := mustNewCode(t, issuer, ctx)

	ac, ok := redeemer.takeCode(ctx, code)
	if !ok {
		t.Fatal("a sibling instance must be able to redeem the code")
	}
	if ac.Email != "user@example.com" || ac.ClientID != "client-1" || ac.CodeChallenge != "challenge" {
		t.Fatalf("payload did not survive the round trip: %+v", ac)
	}

	// Single use has to hold across instances too, including back on the
	// issuer, whose in-memory copy is now stale.
	if _, ok := redeemer.takeCode(ctx, code); ok {
		t.Fatal("redeemed code must not be redeemable twice on the same instance")
	}
	if _, ok := issuer.takeCode(ctx, code); ok {
		t.Fatal("a code redeemed elsewhere must not still work on the issuer")
	}
}

// An expired code in the store must be refused the same way an expired
// in-memory one is — otherwise persistence would quietly extend the TTL.
func TestMCPOAuthState_StoredCodeExpiryEnforced(t *testing.T) {
	ctx := context.Background()
	backing := storetest.New(t, 0)
	issuer := newMCPOAuthState(backing)

	code := mustNewCode(t, issuer, ctx)
	stale := mcpAuthCode{Email: "user@example.com", Expires: time.Now().Add(-time.Minute)}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = backing.WriteRaw(ctx, mcpOAuthCodeKey(code), string(body), "application/json", nil)
	if err != nil {
		t.Fatalf("overwrite stored code: %v", err)
	}

	if _, ok := newMCPOAuthState(backing).takeCode(ctx, code); ok {
		t.Fatal("expired stored code must not be honoured")
	}
}

// Persisting codes must not trade the in-memory leak for a bucket leak:
// abandoned code objects are swept the next time one is issued.
func TestMCPOAuthState_StoredCodesSwept(t *testing.T) {
	ctx := context.Background()
	backing := storetest.New(t, 0)
	st := newMCPOAuthState(backing)

	abandoned := mustNewCode(t, st, ctx)
	stale := mcpAuthCode{Email: "user@example.com", Expires: time.Now().Add(-time.Minute)}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = backing.WriteRaw(ctx, mcpOAuthCodeKey(abandoned), string(body), "application/json", nil)
	if err != nil {
		t.Fatalf("age the stored code: %v", err)
	}

	live := mustNewCode(t, st, ctx)

	keys, err := backing.ListPrefix(ctx, mcpOAuthCodePrefix)
	if err != nil {
		t.Fatalf("list codes: %v", err)
	}
	if len(keys) != 1 || keys[0] != mcpOAuthCodeKey(live) {
		t.Fatalf("stored codes = %v, want only the live one (%s)", keys, mcpOAuthCodeKey(live))
	}
}

func TestMCPOAuthState_ClientRegistration(t *testing.T) {
	ctx := context.Background()
	st := newMCPOAuthState(storetest.New(t, 0))
	id, err := st.registerClient(ctx, []string{"https://cb/one", "https://cb/two"}, "Two Callbacks")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}
	if id == "" {
		t.Fatal("registerClient should return a non-empty id")
	}

	client, ok, err := st.client(ctx, id)
	if err != nil {
		t.Fatalf("client lookup: %v", err)
	}
	if !ok {
		t.Fatal("registered client should be retrievable")
	}
	if !client.allows("https://cb/one") || !client.allows("https://cb/two") {
		t.Fatal("registered redirect URIs should be allowed")
	}
	if client.allows("https://evil/cb") {
		t.Fatal("unregistered redirect URI must not be allowed")
	}
	if _, ok, _ := st.client(ctx, "nope"); ok {
		t.Fatal("unknown client id should not resolve")
	}
}

// TestMCPOAuthState_ClientSurvivesRestart is the regression that motivated
// persisting registrations: an MCP client registers once and reuses its
// client_id for months, so a process-local map meant every redeploy broke
// /oauth/authorize with "unknown client_id". A fresh state over the same store
// must still resolve the id.
func TestMCPOAuthState_ClientSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	backing := storetest.New(t, 0)

	id, err := newMCPOAuthState(backing).registerClient(ctx, []string{"https://cb/one"}, "Restart Probe")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}

	restarted := newMCPOAuthState(backing)
	client, ok, err := restarted.client(ctx, id)
	if err != nil {
		t.Fatalf("client lookup after restart: %v", err)
	}
	if !ok {
		t.Fatal("client_id should still resolve after a restart")
	}
	if !client.allows("https://cb/one") {
		t.Fatal("redirect URIs should survive the restart")
	}
}

// TestMCPOAuthState_ListAndRevoke covers the admin console's two operations.
// The load-bearing assertion is the last one: revoke has to evict the read
// cache, or this instance keeps honouring a client_id whose record is gone.
func TestMCPOAuthState_ListAndRevoke(t *testing.T) {
	ctx := context.Background()
	st := newMCPOAuthState(storetest.New(t, 0))

	keep, err := st.registerClient(ctx, []string{"https://cb/keep"}, "Keeper")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}
	drop, err := st.registerClient(ctx, []string{"https://cb/drop"}, "Doomed")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}

	listed, err := st.listClients(ctx)
	if err != nil {
		t.Fatalf("listClients: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d clients, want 2", len(listed))
	}
	byID := map[string]registeredClient{}
	for _, cl := range listed {
		byID[cl.ID] = cl
	}
	if got := byID[keep]; got.ClientName != "Keeper" || got.Created.IsZero() {
		t.Fatalf("listed record lost its metadata: %+v", got)
	}

	// Both ids are in the read cache right now (registerClient seeds it), which
	// is exactly the condition revoke has to defeat.
	err = st.revokeClient(ctx, drop)
	if err != nil {
		t.Fatalf("revokeClient: %v", err)
	}

	listed, err = st.listClients(ctx)
	if err != nil {
		t.Fatalf("listClients after revoke: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != keep {
		t.Fatalf("after revoke listed %+v, want only %s", listed, keep)
	}
	if _, ok, _ := st.client(ctx, drop); ok {
		t.Fatal("revoked client_id still resolves — the read cache was not evicted")
	}
	if _, ok, _ := st.client(ctx, keep); !ok {
		t.Fatal("revoking one client must not affect the others")
	}
}

// A client_id from the query string lands in a bucket key, so traversal-shaped
// input must be refused before it reaches the store.
func TestMCPOAuthState_ClientIDRejectsTraversal(t *testing.T) {
	st := newMCPOAuthState(storetest.New(t, 0))
	for _, id := range []string{"", "../../_auth/invites/tok", "a/b", "with space", "a.json"} {
		_, ok, err := st.client(context.Background(), id)
		if ok || err != nil {
			t.Fatalf("client(%q) = ok:%v err:%v, want false/nil", id, ok, err)
		}
	}
}
