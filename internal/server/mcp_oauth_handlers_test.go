package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// newOAuthTestServer builds the minimal *Server the OAuth handlers touch:
// the in-memory authorization-server state, a signing secret, and a real
// auth.Auth (in-memory store) so the authorize handler's session check can be
// driven both unauthenticated and with an injected session cookie.
func newOAuthTestServer(t *testing.T) *Server {
	t.Helper()
	a, err := auth.New(auth.Config{
		Store:           storetest.New(t, 0),
		Domain:          "localhost",
		SuperAdminEmail: "admin@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return &Server{
		auth:      a,
		mcpOAuth:  newMCPOAuthState(),
		mcpSecret: "test-oauth-secret",
	}
}

func oauthGET(t *testing.T, s *Server, handler func(*echo.Context) error, rawQuery string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+rawQuery, nil)
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	err := handler(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

func oauthPOSTForm(t *testing.T, s *Server, handler func(*echo.Context) error, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	err := handler(c)
	if err != nil {
		t.Fatalf("handler returned error: %v", err)
	}
	return rec
}

func oauthErrField(t *testing.T, body []byte) string {
	t.Helper()
	var resp map[string]any
	err := json.Unmarshal(body, &resp)
	if err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, body)
	}
	e, _ := resp["error"].(string)
	return e
}

// pkcePair returns a verifier and its S256 challenge.
func pkcePair() (verifier, challenge string) {
	verifier = "test-verifier-0123456789-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

// TestMCPTokenHandler_ErrorPaths pins the RFC 6749 error JSON for every
// rejection branch of the token endpoint — the security-critical lines of the
// whole OAuth flow, previously only covered at the state-machine level.
func TestMCPTokenHandler_ErrorPaths(t *testing.T) {
	s := newOAuthTestServer(t)
	verifier, challenge := pkcePair()
	clientID := s.mcpOAuth.registerClient([]string{"https://cb.example/done"})

	seedCode := func() string {
		return s.mcpOAuth.newCode("user@example.com", clientID, "https://cb.example/done", challenge)
	}

	cases := []struct {
		name    string
		form    url.Values
		wantErr string
	}{
		{
			name:    "wrong grant_type",
			form:    url.Values{"grant_type": {"client_credentials"}},
			wantErr: "unsupported_grant_type",
		},
		{
			name: "unknown code",
			form: url.Values{
				"grant_type": {"authorization_code"}, "code": {"no-such-code"},
				"client_id": {clientID}, "redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
			},
			wantErr: "invalid_grant",
		},
		{
			name: "client_id mismatch",
			form: url.Values{
				"grant_type": {"authorization_code"}, "code": {seedCode()},
				"client_id": {"someone-else"}, "redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
			},
			wantErr: "invalid_grant",
		},
		{
			name: "redirect_uri mismatch",
			form: url.Values{
				"grant_type": {"authorization_code"}, "code": {seedCode()},
				"client_id": {clientID}, "redirect_uri": {"https://evil.example/cb"}, "code_verifier": {verifier},
			},
			wantErr: "invalid_grant",
		},
		{
			name: "PKCE failure",
			form: url.Values{
				"grant_type": {"authorization_code"}, "code": {seedCode()},
				"client_id": {clientID}, "redirect_uri": {"https://cb.example/done"}, "code_verifier": {"wrong-verifier"},
			},
			wantErr: "invalid_grant",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := oauthPOSTForm(t, s, s.mcpTokenHandler, tc.form)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if got := oauthErrField(t, rec.Body.Bytes()); got != tc.wantErr {
				t.Errorf("error = %q, want %q", got, tc.wantErr)
			}
		})
	}
}

// TestMCPTokenHandler_SuccessAndReplay drives the happy path — a correct
// exchange returns a Bearer token the MCP verifier accepts — and then asserts
// the code cannot be replayed, even with otherwise-valid parameters.
func TestMCPTokenHandler_SuccessAndReplay(t *testing.T) {
	s := newOAuthTestServer(t)
	verifier, challenge := pkcePair()
	clientID := s.mcpOAuth.registerClient([]string{"https://cb.example/done"})
	code := s.mcpOAuth.newCode("user@example.com", clientID, "https://cb.example/done", challenge)

	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {clientID}, "redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
	}
	rec := oauthPOSTForm(t, s, s.mcpTokenHandler, form)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if resp.TokenType != "Bearer" || resp.AccessToken == "" || resp.ExpiresIn <= 0 {
		t.Fatalf("token response incomplete: %+v", resp)
	}

	// The minted token must verify with the same secret and carry the email.
	info, err := auth.MCPTokenVerifier(s.mcpSecret)(context.Background(), resp.AccessToken, nil)
	if err != nil {
		t.Fatalf("minted token failed verification: %v", err)
	}
	if info.UserID != "user@example.com" {
		t.Fatalf("token subject = %q, want user@example.com", info.UserID)
	}

	// Replay: the code was consumed; an identical second exchange must fail.
	rec = oauthPOSTForm(t, s, s.mcpTokenHandler, form)
	if rec.Code != http.StatusBadRequest || oauthErrField(t, rec.Body.Bytes()) != "invalid_grant" {
		t.Fatalf("code replay: status=%d body=%s, want 400 invalid_grant", rec.Code, rec.Body.String())
	}
}

// TestMCPAuthorizeHandler_Rejections covers the authorize endpoint's guard
// rails, most importantly that a redirect_uri outside the client's registered
// set is refused — the open-redirect line that dynamic client registration
// leans on.
func TestMCPAuthorizeHandler_Rejections(t *testing.T) {
	s := newOAuthTestServer(t)
	_, challenge := pkcePair()
	clientID := s.mcpOAuth.registerClient([]string{"https://cb.example/done"})

	base := url.Values{
		"response_type": {"code"}, "code_challenge_method": {"S256"},
		"code_challenge": {challenge}, "client_id": {clientID},
		"redirect_uri": {"https://cb.example/done"},
	}
	mutate := func(k, v string) string {
		q := url.Values{}
		for key, vals := range base {
			q[key] = vals
		}
		q.Set(k, v)
		return q.Encode()
	}

	cases := []struct {
		name     string
		rawQuery string
		wantBody string
	}{
		{"wrong response_type", mutate("response_type", "token"), "unsupported response_type"},
		{"plain challenge method", mutate("code_challenge_method", "plain"), "must be S256"},
		{"missing challenge", mutate("code_challenge", ""), "code_challenge is required"},
		{"unknown client", mutate("client_id", "nope"), "unknown client_id"},
		{"unregistered redirect_uri", mutate("redirect_uri", "https://evil.example/cb"), "not registered"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := oauthGET(t, s, s.mcpAuthorizeHandler, tc.rawQuery)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body %q does not mention %q", rec.Body.String(), tc.wantBody)
			}
		})
	}
}

// TestMCPAuthorizeHandler_SessionFlow exercises both halves of the human-auth
// step: no session cookie bounces to /login with the original URL preserved,
// and a valid passkey session issues a code that the token endpoint redeems.
func TestMCPAuthorizeHandler_SessionFlow(t *testing.T) {
	s := newOAuthTestServer(t)
	verifier, challenge := pkcePair()
	clientID := s.mcpOAuth.registerClient([]string{"https://cb.example/done"})
	query := url.Values{
		"response_type": {"code"}, "code_challenge_method": {"S256"},
		"code_challenge": {challenge}, "client_id": {clientID},
		"redirect_uri": {"https://cb.example/done"}, "state": {"opaque-state"},
	}.Encode()

	// Unauthenticated: bounce to /login carrying the return URL.
	rec := oauthGET(t, s, s.mcpAuthorizeHandler, query)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauthenticated status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/login?return=") {
		t.Fatalf("unauthenticated redirect = %q, want /login?return=...", loc)
	}

	// Authenticated: inject a session and present its cookie.
	token, err := s.auth.InjectTestSession(context.Background(), "owner@example.com", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("inject session: %v", err)
	}
	rec = oauthGET(t, s, s.mcpAuthorizeHandler, query,
		&http.Cookie{Name: s.auth.SessionCookieName(), Value: token})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authenticated status = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	dest, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	if dest.Scheme+"://"+dest.Host+dest.Path != "https://cb.example/done" {
		t.Fatalf("redirected to %q, want the registered redirect_uri", dest.String())
	}
	if dest.Query().Get("state") != "opaque-state" {
		t.Fatalf("state not round-tripped: %q", dest.RawQuery)
	}
	code := dest.Query().Get("code")
	if code == "" {
		t.Fatal("no code in redirect")
	}

	// The issued code redeems at the token endpoint with the matching verifier.
	tokenRec := oauthPOSTForm(t, s, s.mcpTokenHandler, url.Values{
		"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {clientID}, "redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
	})
	if tokenRec.Code != http.StatusOK {
		t.Fatalf("token exchange after authorize = %d (%s)", tokenRec.Code, tokenRec.Body.String())
	}
}

// FuzzVerifyPKCE asserts the S256 check never panics on arbitrary inputs and
// never passes unless the challenge is exactly base64url(sha256(verifier)).
func FuzzVerifyPKCE(f *testing.F) {
	verifier, challenge := pkcePair()
	f.Add(verifier, challenge)
	f.Add("", "")
	f.Add("a", "b")
	f.Fuzz(func(t *testing.T, v, c string) {
		got := verifyPKCE(v, c)
		sum := sha256.Sum256([]byte(v))
		want := v != "" && c != "" && base64.RawURLEncoding.EncodeToString(sum[:]) == c
		if got != want {
			t.Fatalf("verifyPKCE(%q, %q) = %v, want %v", v, c, got, want)
		}
	})
}
