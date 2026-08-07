package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth/blob"
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

// oauthTestState returns a state with a namespace unique to this test. The
// listing paths (listClients, sweepStoredCodes) treat everything under the
// prefix as theirs, which is true in production and false when the suite runs
// against a shared Minio bucket — without this, these tests read records left
// by their neighbours and by earlier runs.
func oauthTestState(t *testing.T, s blob.Blobs) *mcpOAuthState {
	t.Helper()
	return newState(s, oauthTestPrefix(t))
}

// oauthTestPrefix is a namespace unique per test, for the cases that need
// several states sharing one.
func oauthTestPrefix(t *testing.T) string {
	t.Helper()
	return "_auth/oauth-test/" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.FormatInt(int64(oauthPrefixSeq.Add(1)), 10) + "/"
}

// Wall-clock alone collides: these tests run in well under a nanosecond's
// worth of timer resolution on a fast machine.
var oauthPrefixSeq atomic.Int32

func TestMCPOAuthState_CodeSingleUse(t *testing.T) {
	ctx := context.Background()
	st := oauthTestState(t, blob.NewMemory())
	code := mustNewCode(t, st, ctx)

	ac, ok, err := st.takeCode(ctx, code)
	if err != nil {
		t.Fatalf("takeCode: %v", err)
	}
	if !ok {
		t.Fatal("first takeCode should succeed")
	}
	if ac.Email != "user@example.com" || ac.ClientID != "client-1" || ac.RedirectURI != "https://cb" {
		t.Fatalf("code payload mismatch: %+v", ac)
	}
	if _, ok, _ := st.takeCode(ctx, code); ok {
		t.Fatal("second takeCode should fail (single use)")
	}
}

// TestMCPOAuthState_CodeRedeemableAcrossInstances is the multi-instance case:
// /oauth/authorize lands on one instance and the /oauth/token POST is
// load-balanced to another, which has never seen the code in memory. Two
// states over one store stand in for the two processes.
func TestMCPOAuthState_CodeRedeemableAcrossInstances(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	// Both instances share a namespace: they stand in for two processes
	// serving the same deployment.
	prefix := oauthTestPrefix(t)
	issuer := newState(backing, prefix)
	redeemer := newState(backing, prefix)

	code := mustNewCode(t, issuer, ctx)

	ac, ok, err := redeemer.takeCode(ctx, code)
	if err != nil {
		t.Fatalf("takeCode: %v", err)
	}
	if !ok {
		t.Fatal("a sibling instance must be able to redeem the code")
	}
	if ac.Email != "user@example.com" || ac.ClientID != "client-1" || ac.CodeChallenge != "challenge" {
		t.Fatalf("payload did not survive the round trip: %+v", ac)
	}

	// Single use has to hold across instances too, including back on the
	// issuer, whose in-memory copy is now stale.
	if _, ok, _ := redeemer.takeCode(ctx, code); ok {
		t.Fatal("redeemed code must not be redeemable twice on the same instance")
	}
	if _, ok, _ := issuer.takeCode(ctx, code); ok {
		t.Fatal("a code redeemed elsewhere must not still work on the issuer")
	}
}

// TestMCPOAuthState_ConcurrentRedemptionYieldsOneToken is the race that used
// to be a documented shortcut. N instances redeem the same code simultaneously
// — the retry storm a flaky network produces — and exactly one may be handed
// the grant. Read-then-delete passes the sequential single-use test and fails
// this one, which is why the sequential test alone was never enough.
func TestMCPOAuthState_ConcurrentRedemptionYieldsOneToken(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	prefix := oauthTestPrefix(t)
	code := mustNewCode(t, newState(backing, prefix), ctx)

	// Separate states over one store: distinct processes, no shared memory to
	// accidentally serialise them.
	const redeemers = 8
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		granted   int
		hardError error
	)
	for range redeemers {
		// Same namespace for every redeemer — a per-state prefix would have
		// them racing on nothing and the test would pass vacuously.
		st := newState(backing, prefix)
		wg.Add(1)
		go func() {
			defer wg.Done()
			ac, ok, err := st.takeCode(ctx, code)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				hardError = err
				return
			}
			if ok {
				granted++
				if ac.Email != "user@example.com" {
					hardError = fmt.Errorf("winner got a mangled payload: %+v", ac)
				}
			}
		}()
	}
	wg.Wait()

	if hardError != nil {
		t.Fatalf("redemption failed: %v", hardError)
	}
	if granted != 1 {
		t.Fatalf("%d redeemers were granted the code, want exactly 1", granted)
	}
}

// A code already claimed must stay refused even if its tombstone is still in
// the bucket — the delete after a successful claim is cleanup, not the thing
// enforcing single use.
func TestMCPOAuthState_ConsumedTombstoneNeverHonoured(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	st := oauthTestState(t, backing)
	code := mustNewCode(t, st, ctx)

	tombstone := mcpAuthCode{
		Email:    "user@example.com",
		Expires:  time.Now().Add(time.Hour),
		Consumed: true,
	}
	body, err := json.Marshal(tombstone)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = backing.Put(ctx, st.codeKey(code), string(body))
	if err != nil {
		t.Fatalf("write tombstone: %v", err)
	}

	if _, ok, _ := st.takeCode(ctx, code); ok {
		t.Fatal("a consumed code must never be honoured again, tombstone or not")
	}
}

// An expired code in the store must be refused the same way an expired
// in-memory one is — otherwise persistence would quietly extend the TTL.
func TestMCPOAuthState_StoredCodeExpiryEnforced(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	prefix := oauthTestPrefix(t)
	issuer := newState(backing, prefix)

	code := mustNewCode(t, issuer, ctx)
	stale := mcpAuthCode{Email: "user@example.com", Expires: time.Now().Add(-time.Minute)}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = backing.Put(ctx, issuer.codeKey(code), string(body))
	if err != nil {
		t.Fatalf("overwrite stored code: %v", err)
	}

	if _, ok, _ := newState(backing, prefix).takeCode(ctx, code); ok {
		t.Fatal("expired stored code must not be honoured")
	}
}

// Persisting codes must not trade the in-memory leak for a bucket leak:
// abandoned code objects are swept the next time one is issued.
func TestMCPOAuthState_StoredCodesSwept(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	st := oauthTestState(t, backing)

	abandoned := mustNewCode(t, st, ctx)
	stale := mcpAuthCode{Email: "user@example.com", Expires: time.Now().Add(-time.Minute)}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = backing.Put(ctx, st.codeKey(abandoned), string(body))
	if err != nil {
		t.Fatalf("age the stored code: %v", err)
	}

	live := mustNewCode(t, st, ctx)

	keys, err := backing.List(ctx, st.codesPrefix())
	if err != nil {
		t.Fatalf("list codes: %v", err)
	}
	if len(keys) != 1 || keys[0] != st.codeKey(live) {
		t.Fatalf("stored codes = %v, want only the live one (%s)", keys, st.codeKey(live))
	}
}

func TestMCPOAuthState_ClientRegistration(t *testing.T) {
	ctx := context.Background()
	st := oauthTestState(t, blob.NewMemory())
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
	backing := blob.NewMemory()

	prefix := oauthTestPrefix(t)
	id, err := newState(backing, prefix).registerClient(ctx, []string{"https://cb/one"}, "Restart Probe")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}

	restarted := newState(backing, prefix)
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
func TestMCPOAuthState_ListAndRevoke(t *testing.T) {
	ctx := context.Background()
	st := oauthTestState(t, blob.NewMemory())

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
		t.Fatal("revoked client_id still resolves")
	}
	if _, ok, _ := st.client(ctx, keep); !ok {
		t.Fatal("revoking one client must not affect the others")
	}
}

// TestMCPOAuthState_RevokeTakesEffectOnOtherInstances is why client() is not
// cached. An admin revokes on whichever instance served the console; every
// other instance has to stop honouring the id on its very next authorize, not
// whenever that process happens to restart.
func TestMCPOAuthState_RevokeTakesEffectOnOtherInstances(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	prefix := oauthTestPrefix(t)
	serving := newState(backing, prefix)
	console := newState(backing, prefix)

	id, err := serving.registerClient(ctx, []string{"https://cb/one"}, "Doomed")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}
	// Resolve it first: this is the read that a cache would have populated.
	if _, ok, _ := serving.client(ctx, id); !ok {
		t.Fatal("client should resolve before revocation")
	}

	err = console.revokeClient(ctx, id)
	if err != nil {
		t.Fatalf("revokeClient: %v", err)
	}

	if _, ok, _ := serving.client(ctx, id); ok {
		t.Fatal("a revoked client_id must stop resolving on every instance immediately")
	}
}

// revokeClient must not report success for an id that could never name a
// registration, or the console shows "revoked" for something it never touched.
func TestMCPOAuthState_RevokeRejectsInvalidID(t *testing.T) {
	st := oauthTestState(t, blob.NewMemory())
	for _, id := range []string{"", "../../_auth/invites/tok", "a/b", "with space"} {
		err := st.revokeClient(context.Background(), id)
		if !errors.Is(err, ErrClientNotFound) {
			t.Errorf("revokeClient(%q) = %v, want ErrClientNotFound", id, err)
		}
	}
}

// listClients must not surface keys that revokeClient would refuse: a row whose
// Revoke button silently does nothing is worse than no row.
func TestMCPOAuthState_ListSkipsNonRegistrationKeys(t *testing.T) {
	ctx := context.Background()
	backing := blob.NewMemory()
	st := oauthTestState(t, backing)

	genuine, err := st.registerClient(ctx, []string{"https://cb/one"}, "Real")
	if err != nil {
		t.Fatalf("registerClient: %v", err)
	}
	err = backing.Put(ctx, st.clientsPrefix()+"not a client id.json", `{"redirect_uris":["https://x"]}`)
	if err != nil {
		t.Fatalf("seed junk key: %v", err)
	}

	listed, err := st.listClients(ctx)
	if err != nil {
		t.Fatalf("listClients: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != genuine {
		t.Fatalf("listed %+v, want only the real registration %s", listed, genuine)
	}
}

// A client_id from the query string lands in a bucket key, so traversal-shaped
// input must be refused before it reaches the store.
func TestMCPOAuthState_ClientIDRejectsTraversal(t *testing.T) {
	st := oauthTestState(t, blob.NewMemory())
	for _, id := range []string{"", "../../_auth/invites/tok", "a/b", "with space", "a.json"} {
		_, ok, err := st.client(context.Background(), id)
		if ok || err != nil {
			t.Fatalf("client(%q) = ok:%v err:%v, want false/nil", id, ok, err)
		}
	}
}
