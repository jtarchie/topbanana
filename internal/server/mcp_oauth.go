package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/jtarchie/topbanana/internal/auth"
)

// This file is a minimal OAuth 2.1 authorization server that fronts the MCP
// endpoint. An MCP client (Claude Code) discovers it via the well-known
// metadata, dynamically registers, then runs the authorization-code + PKCE
// flow. The human-authentication step reuses the existing passkey session:
// /oauth/authorize only issues a code once the browser carries a logged-in
// session, so no second login system is introduced. Tokens are the JWTs minted
// by internal/auth/mcp.go and verified by the bearer middleware on /mcp.
//
// Client registrations and pending authorization codes live in memory
// (process-local). That's fine for a single instance; a multi-instance
// deployment behind a load balancer would need shared storage here.

const mcpAuthCodeTTL = 10 * time.Minute

// echo's response methods return an error wrapcheck flags at every call site.
// These thin wrappers carry the single nolint so the handlers below stay clean
// and consistent (the same pattern the rest of the package uses inline).
func mcpRespJSON(c *echo.Context, code int, v any) error {
	return c.JSON(code, v) //nolint:wrapcheck
}

func mcpRespString(c *echo.Context, code int, msg string) error {
	return c.String(code, msg) //nolint:wrapcheck
}

func mcpRedirect(c *echo.Context, dest string) error {
	return c.Redirect(http.StatusSeeOther, dest) //nolint:wrapcheck
}

// mcpOAuthState holds the in-memory authorization-server state.
type mcpOAuthState struct {
	mu      sync.Mutex
	clients map[string]mcpOAuthClient
	codes   map[string]mcpAuthCode
}

type mcpOAuthClient struct {
	RedirectURIs []string
}

type mcpAuthCode struct {
	Email         string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Expires       time.Time
}

func newMCPOAuthState() *mcpOAuthState {
	return &mcpOAuthState{
		clients: map[string]mcpOAuthClient{},
		codes:   map[string]mcpAuthCode{},
	}
}

func mcpRandomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (st *mcpOAuthState) registerClient(redirectURIs []string) string {
	id := mcpRandomToken()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.clients[id] = mcpOAuthClient{RedirectURIs: redirectURIs}
	return id
}

func (st *mcpOAuthState) client(id string) (mcpOAuthClient, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	c, ok := st.clients[id]
	return c, ok
}

func (st *mcpOAuthState) newCode(email, clientID, redirectURI, challenge string) string {
	code := mcpRandomToken()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.codes[code] = mcpAuthCode{
		Email:         email,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: challenge,
		Expires:       time.Now().Add(mcpAuthCodeTTL),
	}
	return code
}

// takeCode looks up and removes a code (single use). Returns false if missing
// or expired.
func (st *mcpOAuthState) takeCode(code string) (mcpAuthCode, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	ac, ok := st.codes[code]
	if !ok {
		return mcpAuthCode{}, false
	}
	delete(st.codes, code)
	if time.Now().After(ac.Expires) {
		return mcpAuthCode{}, false
	}
	return ac, true
}

func (c mcpOAuthClient) allows(redirectURI string) bool {
	for _, u := range c.RedirectURIs {
		if u == redirectURI {
			return true
		}
	}
	return false
}

// mcpBaseURL is the externally-reachable origin the OAuth metadata advertises.
// Derived from the configured domain/port so it matches what the bearer
// middleware pins as the resource metadata URL. Local-dev (loopback) domains
// get http; everything else https.
func (s *Server) mcpBaseURL() string {
	host := stripPort(s.domain)
	if fallThroughHosts[host] {
		base := "http://" + s.domain
		if s.port != "" && s.port != "80" {
			base += ":" + s.port
		}
		return base
	}
	return "https://" + s.domain
}

// mountMCP registers the OAuth endpoints, the well-known metadata, and the
// bearer-protected MCP endpoint. Called from New only when an MCP secret is set.
func (s *Server) mountMCP(e *echo.Echo) {
	e.GET("/.well-known/oauth-protected-resource", s.mcpProtectedResourceHandler)
	e.GET("/.well-known/oauth-authorization-server", s.mcpAuthServerMetadataHandler)
	e.POST("/oauth/register", s.mcpRegisterHandler)
	e.GET("/oauth/authorize", s.mcpAuthorizeHandler)
	e.POST("/oauth/token", s.mcpTokenHandler)

	verifier := auth.MCPTokenVerifier(s.mcpSecret)
	protected := mcpauth.RequireBearerToken(verifier, &mcpauth.RequireBearerTokenOptions{
		ResourceMetadataURL: s.mcpBaseURL() + "/.well-known/oauth-protected-resource",
		Scopes:              []string{auth.MCPScope},
	})(s.newMCPHandler())
	e.Any("/mcp", echo.WrapHandler(protected))
	e.Any("/mcp/*", echo.WrapHandler(protected))
}

// --- well-known metadata ----------------------------------------------------

func (s *Server) mcpProtectedResourceHandler(c *echo.Context) error {
	base := s.mcpBaseURL()
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{auth.MCPScope},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) mcpAuthServerMetadataHandler(c *echo.Context) error {
	base := s.mcpBaseURL()
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{auth.MCPScope},
	})
}

// --- dynamic client registration (RFC 7591, minimal) ------------------------

func (s *Server) mcpRegisterHandler(c *echo.Context) error {
	var req struct {
		RedirectURIs []string `json:"redirect_uris"`
		ClientName   string   `json:"client_name"`
	}
	err := json.NewDecoder(c.Request().Body).Decode(&req)
	if err != nil {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_client_metadata"})
	}
	if len(req.RedirectURIs) == 0 {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_redirect_uri"})
	}
	clientID := s.mcpOAuth.registerClient(req.RedirectURIs)
	return mcpRespJSON(c, http.StatusCreated, map[string]any{
		"client_id":                  clientID,
		"redirect_uris":              req.RedirectURIs,
		"client_name":                req.ClientName,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code"},
		"response_types":             []string{"code"},
	})
}

// --- authorization endpoint -------------------------------------------------

func (s *Server) mcpAuthorizeHandler(c *echo.Context) error {
	q := c.Request().URL.Query()
	if q.Get("response_type") != "code" {
		return mcpRespString(c, http.StatusBadRequest, "unsupported response_type (want code)")
	}
	if q.Get("code_challenge_method") != "S256" {
		return mcpRespString(c, http.StatusBadRequest, "code_challenge_method must be S256")
	}
	challenge := q.Get("code_challenge")
	if challenge == "" {
		return mcpRespString(c, http.StatusBadRequest, "code_challenge is required")
	}
	clientID := q.Get("client_id")
	client, ok := s.mcpOAuth.client(clientID)
	if !ok {
		return mcpRespString(c, http.StatusBadRequest, "unknown client_id")
	}
	redirectURI := q.Get("redirect_uri")
	if !client.allows(redirectURI) {
		return mcpRespString(c, http.StatusBadRequest, "redirect_uri not registered for this client")
	}

	// Reuse the passkey session for human authentication. If the browser
	// isn't signed in, bounce to /login; the user signs in and re-initiates
	// the connection (their session cookie then satisfies this check).
	email, ok := s.currentSessionEmail(c)
	if !ok {
		return mcpRedirect(c, "/login?return="+url.QueryEscape(c.Request().URL.String()))
	}

	code := s.mcpOAuth.newCode(email, clientID, redirectURI, challenge)
	dest, err := url.Parse(redirectURI)
	if err != nil {
		return mcpRespString(c, http.StatusBadRequest, "invalid redirect_uri")
	}
	rq := dest.Query()
	rq.Set("code", code)
	if state := q.Get("state"); state != "" {
		rq.Set("state", state)
	}
	dest.RawQuery = rq.Encode()
	return mcpRedirect(c, dest.String())
}

// --- token endpoint ---------------------------------------------------------

func (s *Server) mcpTokenHandler(c *echo.Context) error {
	if c.FormValue("grant_type") != "authorization_code" {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
	ac, ok := s.mcpOAuth.takeCode(c.FormValue("code"))
	if !ok {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}
	if ac.ClientID != c.FormValue("client_id") || ac.RedirectURI != c.FormValue("redirect_uri") {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}
	if !verifyPKCE(c.FormValue("code_verifier"), ac.CodeChallenge) {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{
			"error":             "invalid_grant",
			"error_description": "PKCE verification failed",
		})
	}
	token, err := auth.MintMCPToken(s.mcpSecret, ac.Email, auth.MCPTokenTTL)
	if err != nil {
		return mcpRespJSON(c, http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"access_token": token,
		"token_type":   "Bearer",
		"expires_in":   int(auth.MCPTokenTTL.Seconds()),
		"scope":        auth.MCPScope,
	})
}

// verifyPKCE checks the S256 challenge: base64url(sha256(verifier)) == challenge.
func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:]) == challenge
}
