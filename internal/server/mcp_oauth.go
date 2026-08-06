package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/photowall"
	"github.com/jtarchie/topbanana/internal/store"
)

// This file is a minimal OAuth 2.1 authorization server that fronts the MCP
// endpoint. An MCP client (Claude Code) discovers it via the well-known
// metadata, dynamically registers, then runs the authorization-code + PKCE
// flow. The human-authentication step reuses the existing passkey session:
// /oauth/authorize only issues a code once the browser carries a logged-in
// session, so no second login system is introduced. Tokens are the JWTs minted
// by internal/auth/mcp.go and verified by the bearer middleware on /mcp.
//
// Client registrations are persisted to S3 under mcpOAuthClientPrefix: an MCP
// client registers once and reuses its client_id indefinitely, so a
// process-local map meant every restart or redeploy broke returning clients
// with "unknown client_id". Pending authorization codes are still in memory
// (process-local) — they carry a 10-minute TTL, so losing them costs at most
// one retry of an in-progress connect, and only that map keeps this endpoint
// from being run multi-instance.

const mcpAuthCodeTTL = 10 * time.Minute

// mcpOAuthClientPrefix is the bucket home of RFC 7591 registrations, one JSON
// blob per client_id — the same shape internal/auth uses for invites.
const mcpOAuthClientPrefix = "_auth/oauth/clients/"

// mcpClientIDPattern is the alphabet mcpRandomToken emits. client_id arrives
// from an untrusted query parameter and is interpolated into a bucket key, so
// anything outside it is rejected before it reaches the store.
var mcpClientIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func mcpOAuthClientKey(id string) string {
	return mcpOAuthClientPrefix + id + ".json"
}

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

// mcpOAuthState holds the authorization-server state: S3-backed client
// registrations (with an in-memory read cache in front) and in-memory codes.
type mcpOAuthState struct {
	store *store.Store

	// registerLimiter throttles /oauth/register per client IP. RFC 7591
	// registration is necessarily unauthenticated — Claude Code registers
	// before any human signs in — so without this an anonymous loop writes
	// bucket objects and cache entries without bound. Nothing is exposed by
	// the spam (a client_id is inert until /oauth/authorize sees a passkey
	// session); this caps the junk and the PUT bill. The type lives in
	// internal/photowall because that's where the first per-key token bucket
	// was needed; it carries no photo-specific behaviour.
	registerLimiter *photowall.Limiter

	mu sync.Mutex
	// clients is a read cache only — S3 is the source of truth, and a miss
	// here falls through to it. Never treat an absent entry as "no such
	// client".
	clients map[string]mcpOAuthClient
	codes   map[string]mcpAuthCode
}

type mcpOAuthClient struct {
	RedirectURIs []string `json:"redirect_uris"`
}

type mcpAuthCode struct {
	Email         string
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Expires       time.Time
}

func newMCPOAuthState(s *store.Store) *mcpOAuthState {
	return &mcpOAuthState{
		store: s,
		// Registering is a once-per-tool-install event, so this is far above
		// any legitimate rate while still blunting a script: ~1 per 5s
		// sustained per IP, burst 5 for a shared NAT.
		registerLimiter: photowall.NewLimiter(0.2, 5),
		clients:         map[string]mcpOAuthClient{},
		codes:           map[string]mcpAuthCode{},
	}
}

func mcpRandomToken() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// registerClient persists a registration and returns its client_id. The write
// must land before the id is handed out: a client that gets an id we failed to
// store would fail at /oauth/authorize with no way to recover but re-register.
func (st *mcpOAuthState) registerClient(ctx context.Context, redirectURIs []string) (string, error) {
	id := mcpRandomToken()
	client := mcpOAuthClient{RedirectURIs: redirectURIs}
	body, err := json.Marshal(client)
	if err != nil {
		return "", fmt.Errorf("server: marshal oauth client: %w", err)
	}
	err = st.store.WriteRaw(ctx, mcpOAuthClientKey(id), string(body), "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("server: write oauth client: %w", err)
	}
	st.cache(id, client)
	return id, nil
}

// client resolves a client_id, reading through the cache to S3. The bool is
// "registered"; a non-nil error means the lookup itself failed and must not be
// reported to the caller as an unknown client.
func (st *mcpOAuthState) client(ctx context.Context, id string) (mcpOAuthClient, bool, error) {
	if !mcpClientIDPattern.MatchString(id) {
		return mcpOAuthClient{}, false, nil
	}
	st.mu.Lock()
	c, ok := st.clients[id]
	st.mu.Unlock()
	if ok {
		return c, true, nil
	}

	obj, err := st.store.ReadRaw(ctx, mcpOAuthClientKey(id))
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("server: read oauth client: %w", err)
	}
	if obj.Content == "" {
		return mcpOAuthClient{}, false, nil
	}
	err = json.Unmarshal([]byte(obj.Content), &c)
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("server: parse oauth client: %w", err)
	}
	st.cache(id, c)
	return c, true, nil
}

func (st *mcpOAuthState) cache(id string, c mcpOAuthClient) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.clients[id] = c
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

	// Binary upload endpoint for create_upload_ticket. Auth lives in the signed
	// ticket carried in the path (the agent can't read its MCP bearer token to
	// set a header), so this route is outside the bearer middleware; BodyLimit
	// is a first-line cap before the handler re-checks the size.
	e.POST("/upload/ticket/:token", s.uploadTicketHandler, middleware.BodyLimit(maxUploadBytes+1024))
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
	if !s.mcpOAuth.registerLimiter.Allow(c.RealIP()) {
		return mcpRespJSON(c, http.StatusTooManyRequests, map[string]string{
			"error":             "temporarily_unavailable",
			"error_description": "too many registration attempts — please retry shortly",
		})
	}
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
	clientID, err := s.mcpOAuth.registerClient(c.Request().Context(), req.RedirectURIs)
	if err != nil {
		return mcpRespJSON(c, http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
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
	client, ok, err := s.mcpOAuth.client(c.Request().Context(), clientID)
	if err != nil {
		return mcpRespString(c, http.StatusInternalServerError, "client lookup failed")
	}
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
