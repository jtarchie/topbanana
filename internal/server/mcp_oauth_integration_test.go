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

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/auth/oauth"
	"github.com/jtarchie/topbanana/internal/blobs"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// The authorization server takes human authentication as a callback, and its
// own tests stub that out — correctly, since the library has no idea what a
// passkey is. This is the seam on the other side: proof that *this* app's
// passkey session actually satisfies oauth.Config.CurrentUser, which is the
// one thing neither package's tests can show alone.
func TestOAuthIntegration_PasskeySessionSatisfiesCurrentUser(t *testing.T) {
	ctx := context.Background()
	backing := storetest.New(t, 0)
	a, err := auth.New(auth.Config{
		Blobs:           blobs.FromStore(backing),
		Domain:          "localhost",
		SuperAdminEmail: "admin@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	srv := &Server{auth: a, domain: "localhost", port: "8080", mcpSecret: "integration-secret", store: backing}
	oa, err := oauth.New(oauth.Config{
		Blobs:       blob.NewMemory(),
		BaseURL:     srv.mcpBaseURL(),
		Secret:      srv.mcpSecret,
		CurrentUser: srv.currentSessionEmail,
	})
	if err != nil {
		t.Fatalf("oauth.New: %v", err)
	}

	e := echo.New()
	oa.Mount(e)

	clientID := registerViaHTTP(t, e)
	verifier := "integration-verifier-0123456789-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	query := url.Values{
		"response_type": {"code"}, "code_challenge_method": {"S256"},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])},
		"client_id":      {clientID}, "redirect_uri": {"https://cb.example/done"},
	}.Encode()

	// No session: the app's CurrentUser says no, so the library bounces.
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query, nil))
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/login?return=") {
		t.Fatalf("unauthenticated = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}

	// A real passkey session cookie must satisfy the same callback.
	token, err := a.InjectTestSession(ctx, "owner@example.com", auth.RoleAdmin)
	if err != nil {
		t.Fatalf("inject session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+query, nil)
	req.AddCookie(&http.Cookie{Name: a.SessionCookieName(), Value: token})
	rec = httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("authenticated = %d, want 303 (%s)", rec.Code, rec.Body.String())
	}
	dest, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse redirect: %v", err)
	}
	code := dest.Query().Get("code")
	if code == "" {
		t.Fatalf("no code issued; redirected to %q", dest)
	}

	// And the code redeems for a token carrying the session's identity —
	// which is the whole point of wiring the two together.
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID},
		"redirect_uri": {"https://cb.example/done"}, "code_verifier": {verifier},
	}
	tokReq := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	tokReq.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	tokRec := httptest.NewRecorder()
	e.ServeHTTP(tokRec, tokReq)
	if tokRec.Code != http.StatusOK {
		t.Fatalf("token exchange = %d (%s)", tokRec.Code, tokRec.Body.String())
	}
	if !strings.Contains(tokRec.Body.String(), "access_token") {
		t.Fatalf("no access_token in %s", tokRec.Body.String())
	}
}

func registerViaHTTP(t *testing.T, e *echo.Echo) string {
	t.Helper()
	body := strings.NewReader(`{"redirect_uris":["https://cb.example/done"],"client_name":"Integration"}`)
	req := httptest.NewRequest(http.MethodPost, "/oauth/register", body)
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register = %d (%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		ClientID string `json:"client_id"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil || resp.ClientID == "" {
		t.Fatalf("decode register response: %v (%s)", err, rec.Body.String())
	}
	return resp.ClientID
}
