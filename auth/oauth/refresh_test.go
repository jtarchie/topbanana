package oauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

// tokenResponse is the subset of the token endpoint's JSON these tests read.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Error        string `json:"error"`
}

// connect runs the full authorization-code exchange and returns the first
// token pair, the way a client does on its initial connect.
func connect(t *testing.T, s *Server) (clientID string, tok tokenResponse) {
	t.Helper()
	clientID = registerTestClient(t, s)
	verifier, challenge := pkcePair()
	signIn("user@example.com")
	code, err := s.st.newCode(context.Background(), "user@example.com", clientID, "https://cb.example/done", challenge)
	if err != nil {
		t.Fatalf("newCode: %v", err)
	}
	rec := oauthPOSTForm(t, s, s.tokenHandler, url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("initial exchange = %d (%s)", rec.Code, rec.Body.String())
	}
	tok = decodeToken(t, rec.Body.Bytes())
	if tok.RefreshToken == "" {
		t.Fatal("authorization_code exchange returned no refresh_token — the client has no way to stay connected past the access token's TTL, which is the whole problem this solves")
	}
	return clientID, tok
}

func decodeToken(t *testing.T, body []byte) tokenResponse {
	t.Helper()
	tok := tokenResponse{}
	err := json.Unmarshal(body, &tok)
	if err != nil {
		t.Fatalf("decode token response: %v (%s)", err, body)
	}
	return tok
}

func refresh(t *testing.T, s *Server, clientID, refreshToken string) (*tokenResponse, int) {
	t.Helper()
	rec := oauthPOSTForm(t, s, s.tokenHandler, url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}, "client_id": {clientID},
	})
	tok := decodeToken(t, rec.Body.Bytes())
	return &tok, rec.Code
}

// The point of the whole feature: a client can renew without a human.
func TestRefresh_RenewsWithoutReauthorizing(t *testing.T) {
	s := newOAuthTestServer(t)
	clientID, first := connect(t, s)

	// Nobody is signed in any more — a refresh must not need a browser.
	signOut()

	tok, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("refresh = %d (%+v)", status, tok)
	}
	if tok.AccessToken == "" {
		t.Fatal("refresh returned no access token")
	}
	if tok.RefreshToken == first.RefreshToken {
		t.Fatal("refresh token was not rotated — OAuth 2.1 requires a new one per use for public clients")
	}
}

// Reuse detection: replaying a spent token means someone has a copy, so the
// whole chain dies — including the link the legitimate client still holds.
func TestRefresh_ReuseRevokesTheWholeChain(t *testing.T) {
	s := newOAuthTestServer(t)
	clientID, first := connect(t, s)

	second, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("first refresh = %d", status)
	}
	third, status := refresh(t, s, clientID, second.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("second refresh = %d", status)
	}

	// An attacker replays the spent first token.
	_, status = refresh(t, s, clientID, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("replay of a spent token succeeded")
	}

	// The legitimate client's current token must now be dead too: the chain
	// is known to have leaked, so continuing to honour it would keep the
	// attacker's copy alive alongside it.
	tok, status := refresh(t, s, clientID, third.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("the live token survived reuse detection — the chain was not revoked, so the leaked copy is still usable")
	}
	if tok.Error != "invalid_grant" {
		t.Fatalf("error = %q, want invalid_grant", tok.Error)
	}
}

// A lost response must not cost the session: the client retries the link it
// still holds and gets the successor it missed, which then keeps working.
func TestRefresh_RetryWithinGraceReturnsSameSuccessor(t *testing.T) {
	s := newOAuthTestServer(t)
	clientID, first := connect(t, s)

	lost, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("first refresh = %d", status)
	}
	retry, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("retry within grace = %d (%+v) — a dropped response revoked the session", status, retry)
	}
	if retry.RefreshToken != lost.RefreshToken {
		t.Fatal("retry minted a second successor; one link must not fork into two live chains")
	}
	_, status = refresh(t, s, clientID, retry.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("successor after retry = %d", status)
	}
}

// Once the successor is spent, the old link's holder is not the client that
// moved on — that is a replay, grace or not.
func TestRefresh_RetryAfterSuccessorSpentIsReuse(t *testing.T) {
	s := newOAuthTestServer(t)
	clientID, first := connect(t, s)

	second, _ := refresh(t, s, clientID, first.RefreshToken)
	third, status := refresh(t, s, clientID, second.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("second refresh = %d", status)
	}
	_, status = refresh(t, s, clientID, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("a link whose successor was already spent was honoured")
	}
	_, status = refresh(t, s, clientID, third.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("chain survived a replay inside the grace window")
	}
}

func TestRefresh_RetryAfterGraceIsReuse(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()
	clientID, first := connect(t, s)

	second, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("first refresh = %d", status)
	}

	// Age the consumption past the grace window.
	key := s.st.refreshKey(first.RefreshToken)
	obj, err := s.st.store.Get(ctx, key)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	rec := refreshRecord{}
	err = json.Unmarshal([]byte(obj.Content), &rec)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rec.ConsumedAt = time.Now().Add(-2 * refreshReuseGrace)
	body, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = s.st.store.Put(ctx, key, string(body))
	if err != nil {
		t.Fatalf("put: %v", err)
	}

	_, status = refresh(t, s, clientID, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("a consumed link was honoured after the grace window")
	}
	_, status = refresh(t, s, clientID, second.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("chain survived a replay after the grace window")
	}
}

// A refresh token is bound to the client it was issued to.
func TestRefresh_RejectsAnotherClientsToken(t *testing.T) {
	s := newOAuthTestServer(t)
	_, first := connect(t, s)
	otherClient := registerTestClient(t, s)

	_, status := refresh(t, s, otherClient, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("a refresh token was honoured for a different client_id")
	}
}

func TestRefresh_ExpiredIsRefused(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()
	clientID, first := connect(t, s)

	// Age the stored record past its TTL.
	stale := refreshRecord{
		Email: "user@example.com", ClientID: clientID,
		Family: "fam", Expires: time.Now().Add(-time.Minute),
	}
	body, err := json.Marshal(stale)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	err = s.st.store.Put(ctx, s.st.refreshKey(first.RefreshToken), string(body))
	if err != nil {
		t.Fatalf("age the record: %v", err)
	}

	_, status := refresh(t, s, clientID, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("an expired refresh token was accepted")
	}
}

// Revoking a registration has to cut off renewal too. Otherwise the console
// reports "revoked" while the client quietly refreshes forever.
func TestRefresh_RevokingClientKillsItsTokens(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()
	clientID, first := connect(t, s)

	err := s.RevokeClient(ctx, clientID)
	if err != nil {
		t.Fatalf("RevokeClient: %v", err)
	}

	_, status := refresh(t, s, clientID, first.RefreshToken)
	if status == http.StatusOK {
		t.Fatal("a revoked client could still refresh — revocation did not actually end its access")
	}
}

// Concurrent refreshes on the same link: exactly one may rotate it. Callers
// that arrive after the claim are grace retries and get that same successor;
// two different successors would be two live chains from one token.
func TestRefresh_ConcurrentUseYieldsOneWinner(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()
	clientID, first := connect(t, s)

	const callers = 6
	results := make(chan string, callers)
	for range callers {
		go func() {
			rec, ok, err := s.st.redeemRefresh(ctx, first.RefreshToken, clientID)
			if !ok || err != nil {
				rec.Successor = ""
			}
			results <- rec.Successor
		}()
	}
	successors := map[string]bool{}
	for range callers {
		if next := <-results; next != "" {
			successors[next] = true
		}
	}
	if len(successors) != 1 {
		t.Fatalf("%d distinct successors from concurrent refreshes, want exactly 1", len(successors))
	}
}

// The metadata has to advertise the grant, or a conformant client will never
// attempt it and will keep prompting the user to reconnect instead.
func TestRefresh_AdvertisedInMetadata(t *testing.T) {
	s := newOAuthTestServer(t)
	rec := oauthGET(t, s, s.authServerMetadataHandler, "")
	if !strings.Contains(rec.Body.String(), `"refresh_token"`) {
		t.Fatalf("grant_types_supported omits refresh_token: %s", rec.Body.String())
	}
}

// Advertising it in the metadata is not enough on its own. RFC 7591 makes the
// registration response the authoritative grant set for the client it was
// issued to, so a library that believes its own registration will never send a
// refresh_token grant when that response omits it — and the symptom is
// indistinguishable from having no refresh support at all: the user goes back
// through the browser every time the access token ages out, while the metadata
// insists renewals are available.
//
// Compared against the metadata rather than a literal, because the two
// disagreeing is the whole failure this pins.
func TestRefresh_AdvertisedInRegistrationResponse(t *testing.T) {
	s := newOAuthTestServer(t)

	body := strings.NewReader(`{"redirect_uris":["https://cb.example/done"],"client_name":"Test Client"}`)
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	err := s.registerHandler(echo.New().NewContext(req, rec))
	if err != nil {
		t.Fatalf("register handler: %v", err)
	}
	if rec.Code != http.StatusCreated {
		t.Fatalf("register returned %d: %s", rec.Code, rec.Body.String())
	}

	var registered struct {
		GrantTypes []string `json:"grant_types"`
	}

	err = json.Unmarshal(rec.Body.Bytes(), &registered)
	if err != nil {
		t.Fatalf("decode registration response: %v", err)
	}

	var metadata struct {
		GrantTypes []string `json:"grant_types_supported"`
	}

	metaBody := oauthGET(t, s, s.authServerMetadataHandler, "").Body.Bytes()

	err = json.Unmarshal(metaBody, &metadata)
	if err != nil {
		t.Fatalf("decode metadata: %v", err)
	}

	if !slices.Equal(registered.GrantTypes, metadata.GrantTypes) {
		t.Fatalf("registration advertises %v but metadata advertises %v; a client trusting its own registration would never renew",
			registered.GrantTypes, metadata.GrantTypes)
	}
	if !slices.Contains(registered.GrantTypes, "refresh_token") {
		t.Fatalf("registration response omits refresh_token: %s", rec.Body.String())
	}
}

// The sweep is a LIST plus a GET per record, and refresh records outlive the
// tokens they mint. Running it on every grant would make each refresh pay for
// every record every other client has ever held, so it is throttled — a
// second grant must not sweep again.
func TestRefresh_SweepIsThrottled(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()
	clientID, first := connect(t, s)
	s.st.sweeps.Wait()

	// A record already past its grace window: the sweep would remove it.
	dead := refreshRecord{
		Email: "user@example.com", ClientID: clientID, Family: "old",
		Expires: time.Now().Add(-2 * refreshTombstoneGrace),
	}
	body, err := json.Marshal(dead)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	deadKey := s.st.refreshKey("deadTokenAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	err = s.st.store.Put(ctx, deadKey, string(body))
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// connect() already swept, so this refresh must skip it and leave the
	// stale record alone.
	_, status := refresh(t, s, clientID, first.RefreshToken)
	if status != http.StatusOK {
		t.Fatalf("refresh = %d", status)
	}
	s.st.sweeps.Wait()
	obj, err := s.st.store.Get(ctx, deadKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if obj.Content == "" {
		t.Fatal("the sweep ran on a second grant inside the throttle window; every refresh would scan every record")
	}

	// Forcing the interval open lets it through, so the throttle delays the
	// sweep rather than disabling it.
	s.st.lastRefreshSweep.Store(0)
	s.st.maybeSweepRefresh(ctx)
	s.st.sweeps.Wait()
	obj, err = s.st.store.Get(ctx, deadKey)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if obj.Content != "" {
		t.Fatal("the sweep never removed an expired record even once the window opened")
	}
}

// A revocation that could not reach the store must not report success: an
// operator told "revoked" while the chain is live has been misinformed about
// who still has access.
func TestRefresh_RevokeRefusesEmptySelectors(t *testing.T) {
	s := newOAuthTestServer(t)
	ctx := context.Background()

	err := s.st.revokeFamily(ctx, "")
	if err == nil {
		t.Fatal("revokeFamily(\"\") returned success — it would match every malformed record and delete them as a group")
	}
	err = s.st.revokeClientRefresh(ctx, "")
	if err == nil {
		t.Fatal("revokeClientRefresh(\"\") returned success")
	}
}
