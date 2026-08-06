package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
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
// Both halves of the flow's state are persisted to S3, because both outlive a
// single request and neither survives being process-local:
//
//   - Client registrations (mcpOAuthClientPrefix). An MCP client registers once
//     and reuses its client_id indefinitely, so a map meant every restart or
//     redeploy broke returning clients with "unknown client_id".
//   - Authorization codes (mcpOAuthCodePrefix), so the /oauth/token POST can
//     land on a different instance than the /oauth/authorize that issued it.
//     Without this a two-instance deployment fails roughly half of all
//     connects, and retrying is a coin flip rather than a fix. There is
//     deliberately no in-memory copy — see newCode.
//
// Single use is enforced by a compare-and-set on the stored code, not by the
// delete that follows it — see takeCode. Two redemptions racing on the same
// code both read the same version and exactly one write can win; the loser is
// refused. Read-then-delete would give both a token, since neither can tell it
// lost.

const mcpAuthCodeTTL = 10 * time.Minute

// mcpOAuthPrefix is the bucket namespace the authorization server owns:
// registrations under clients/, pending authorization codes under codes/, one
// JSON blob each — the same shape internal/auth uses for invites.
//
// Held per-instance rather than as a package constant because both listing
// paths (listClients, sweepStoredCodes) assume everything under the prefix is
// theirs. That is true in production, where the server owns the namespace, and
// false in tests sharing one bucket — which is how a suite that passed against
// the in-memory store failed against Minio, reading records left by its
// neighbours and by previous runs.
const mcpOAuthPrefix = "_auth/oauth/"

func (st *mcpOAuthState) clientsPrefix() string { return st.prefix + "clients/" }
func (st *mcpOAuthState) codesPrefix() string   { return st.prefix + "codes/" }

// mcpTokenPattern is the alphabet mcpRandomToken emits. Both a client_id and an
// authorization code arrive from untrusted request input and are interpolated
// into a bucket key, so anything outside it is rejected before it reaches the
// store.
var mcpTokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func (st *mcpOAuthState) clientKey(id string) string {
	return st.clientsPrefix() + id + ".json"
}

func (st *mcpOAuthState) codeKey(code string) string {
	return st.codesPrefix() + code + ".json"
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

// mcpOAuthState holds the authorization-server state. Both registrations and
// codes live in the store and nowhere else; the only process-local state is the
// registration rate limiter, which is advisory and fine to keep per-instance.
type mcpOAuthState struct {
	store *store.Store

	// prefix is the bucket namespace this instance owns; see mcpOAuthPrefix.
	prefix string

	// registerLimiter throttles /oauth/register per client IP. RFC 7591
	// registration is necessarily unauthenticated — Claude Code registers
	// before any human signs in — so without this an anonymous loop writes
	// bucket objects and cache entries without bound. Nothing is exposed by
	// the spam (a client_id is inert until /oauth/authorize sees a passkey
	// session); this caps the junk and the PUT bill. The type lives in
	// internal/photowall because that's where the first per-key token bucket
	// was needed; it carries no photo-specific behaviour.
	registerLimiter *photowall.Limiter
}

type mcpOAuthClient struct {
	RedirectURIs []string `json:"redirect_uris"`
	// ClientName and Created exist purely so the admin console can show a
	// human something other than a random id. Both are self-reported by an
	// unauthenticated caller (ClientName especially) — display only, never a
	// trust signal. Records written before these fields decode as zero values.
	ClientName string    `json:"client_name,omitempty"`
	Created    time.Time `json:"created,omitempty"`
}

type mcpAuthCode struct {
	Email         string    `json:"email"`
	ClientID      string    `json:"client_id"`
	RedirectURI   string    `json:"redirect_uri"`
	CodeChallenge string    `json:"code_challenge"`
	Expires       time.Time `json:"expires"`
	// Consumed marks a code that has been redeemed. Written by the winning
	// compare-and-set in takeCode, which is what makes redemption single use
	// across instances; the delete that follows is only cleanup. A record
	// carrying it is a tombstone and must never be honoured again.
	Consumed bool `json:"consumed,omitempty"`
}

func newMCPOAuthState(s *store.Store) *mcpOAuthState {
	return newMCPOAuthStateAt(s, mcpOAuthPrefix)
}

// newMCPOAuthStateAt is newMCPOAuthState with an explicit namespace. Tests use
// it to get a prefix of their own so they don't read each other's records.
func newMCPOAuthStateAt(s *store.Store, prefix string) *mcpOAuthState {
	return &mcpOAuthState{
		store:  s,
		prefix: prefix,
		// Registering is a once-per-tool-install event, so this is far above
		// any legitimate rate while still blunting a script: ~1 per 5s
		// sustained per IP, burst 5 for a shared NAT.
		registerLimiter: photowall.NewLimiter(0.2, 5),
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
func (st *mcpOAuthState) registerClient(ctx context.Context, redirectURIs []string, clientName string) (string, error) {
	id := mcpRandomToken()
	client := mcpOAuthClient{
		RedirectURIs: redirectURIs,
		ClientName:   clientName,
		Created:      time.Now().UTC(),
	}
	body, err := json.Marshal(client)
	if err != nil {
		return "", fmt.Errorf("server: marshal oauth client: %w", err)
	}
	err = st.store.WriteRaw(ctx, st.clientKey(id), string(body), "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("server: write oauth client: %w", err)
	}
	return id, nil
}

// client resolves a client_id straight from the store. The bool is
// "registered"; a non-nil error means the lookup itself failed and must not be
// reported to the caller as an unknown client.
//
// Deliberately uncached. A read cache here would save one GET on
// /oauth/authorize — a rare, human-initiated request that is already doing a
// session lookup and a redirect — and would buy that with the one transition
// that matters: revocation. A revoked client_id would keep working on every
// instance that had it cached until that process restarted, while the admin
// console reports it as revoked. Same reasoning as newCode: a cache whose
// staleness grants access isn't a cache.
func (st *mcpOAuthState) client(ctx context.Context, id string) (mcpOAuthClient, bool, error) {
	if !mcpTokenPattern.MatchString(id) {
		return mcpOAuthClient{}, false, nil
	}
	obj, err := st.store.ReadRaw(ctx, st.clientKey(id))
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("server: read oauth client: %w", err)
	}
	if obj.Content == "" {
		return mcpOAuthClient{}, false, nil
	}
	c := mcpOAuthClient{}
	err = json.Unmarshal([]byte(obj.Content), &c)
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("server: parse oauth client: %w", err)
	}
	return c, true, nil
}

// registeredClient pairs a stored registration with its client_id, which lives
// in the key rather than the body.
type registeredClient struct {
	ID string
	mcpOAuthClient
}

// listClients enumerates every registration for the admin console. O(N) reads
// over the prefix, same shape as auth.InviteStore.List — fine at the scale this
// runs at (one record per tool install). Unparseable records are skipped rather
// than failing the page: one bad blob shouldn't hide the rest.
func (st *mcpOAuthState) listClients(ctx context.Context) ([]registeredClient, error) {
	keys, err := st.store.ListPrefix(ctx, st.clientsPrefix())
	if err != nil {
		return nil, fmt.Errorf("server: list oauth clients: %w", err)
	}
	out := make([]registeredClient, 0, len(keys))
	for _, key := range keys {
		id := strings.TrimSuffix(strings.TrimPrefix(key, st.clientsPrefix()), ".json")
		// Every id we ever issued is a mcpRandomToken, so anything else under
		// this prefix is not a registration. Skipping it keeps the console
		// consistent with revokeClient, which refuses such ids — otherwise the
		// page would show a row whose Revoke button silently does nothing.
		if !mcpTokenPattern.MatchString(id) {
			continue
		}
		obj, err := st.store.ReadRaw(ctx, key)
		if err != nil || obj.Content == "" {
			continue
		}
		c := mcpOAuthClient{}
		err = json.Unmarshal([]byte(obj.Content), &c)
		if err != nil {
			continue
		}
		out = append(out, registeredClient{ID: id, mcpOAuthClient: c})
	}
	return out, nil
}

// revokeClient deletes a registration. Because client() reads through to the
// store on every authorize, the delete takes effect immediately and on every
// instance. Returns ErrMCPClientNotFound for an id that was never a valid
// registration, so the console doesn't report a revocation that didn't happen.
func (st *mcpOAuthState) revokeClient(ctx context.Context, id string) error {
	if !mcpTokenPattern.MatchString(id) {
		return ErrMCPClientNotFound
	}
	err := st.store.DeleteRaw(ctx, st.clientKey(id))
	if err != nil {
		return fmt.Errorf("server: revoke oauth client: %w", err)
	}
	return nil
}

// ErrMCPClientNotFound is returned when a revoke targets an id that could never
// name a registration.
var ErrMCPClientNotFound = errors.New("mcp client not found")

// newCode issues an authorization code. The stored object is the code — there
// is deliberately no in-memory copy. A local map looks like a free fast path
// but silently breaks single use: if instance A issues, B redeems, and the
// request retries onto A, A's map still says yes and mints a second token. A
// cache whose staleness grants access isn't a cache.
func (st *mcpOAuthState) newCode(ctx context.Context, email, clientID, redirectURI, challenge string) (string, error) {
	code := mcpRandomToken()
	body, err := json.Marshal(mcpAuthCode{
		Email:         email,
		ClientID:      clientID,
		RedirectURI:   redirectURI,
		CodeChallenge: challenge,
		Expires:       time.Now().Add(mcpAuthCodeTTL),
	})
	if err != nil {
		return "", fmt.Errorf("server: marshal auth code: %w", err)
	}
	err = st.store.WriteRaw(ctx, st.codeKey(code), string(body), "application/json", nil)
	if err != nil {
		return "", fmt.Errorf("server: write auth code: %w", err)
	}
	st.sweepStoredCodes(ctx)
	return code, nil
}

// takeCode looks up and removes a code (single use). ok=false means missing or
// expired, and the caller must not distinguish the two: both are invalid_grant
// and the difference is only useful to someone probing. A non-nil error is
// different in kind — the store failed, so we cannot say whether the code was
// good, and the caller must report that as transient rather than as a verdict
// on the code.
func (st *mcpOAuthState) takeCode(ctx context.Context, code string) (mcpAuthCode, bool, error) {
	if !mcpTokenPattern.MatchString(code) {
		return mcpAuthCode{}, false, nil
	}
	obj, err := st.store.ReadRaw(ctx, st.codeKey(code))
	if err != nil {
		return mcpAuthCode{}, false, fmt.Errorf("server: read auth code: %w", err)
	}
	if obj.Content == "" {
		return mcpAuthCode{}, false, nil
	}
	stored := mcpAuthCode{}
	err = json.Unmarshal([]byte(obj.Content), &stored)
	if err != nil {
		slog.Warn("mcp.oauth.code_parse_failed", "err", err)
		return mcpAuthCode{}, false, nil
	}
	if stored.Consumed || time.Now().After(stored.Expires) {
		return mcpAuthCode{}, false, nil
	}

	// Claim it with a compare-and-set against the version we just read. This is
	// what makes single use hold across instances: two redemptions racing on
	// the same code both reach here, both hold the same ETag, and exactly one
	// write can succeed — the loser gets ErrPrecondition and is refused. A
	// plain read-then-delete gives both of them a token, because neither can
	// tell it lost. A failure to claim must never mint: that is the whole
	// property, and it costs the user one retry with a fresh code.
	stored.Consumed = true
	consumed, err := json.Marshal(stored)
	if err != nil {
		return mcpAuthCode{}, false, fmt.Errorf("server: marshal consumed auth code: %w", err)
	}
	_, err = st.store.WriteRawIfMatch(ctx, st.codeKey(code), string(consumed), "application/json", nil, obj.ETag)
	if errors.Is(err, store.ErrPrecondition) {
		// Someone else claimed it first, or it was swept out from under us.
		return mcpAuthCode{}, false, nil
	}
	if err != nil {
		return mcpAuthCode{}, false, fmt.Errorf("server: claim auth code: %w", err)
	}

	// We hold the claim, so the code can no longer be redeemed by anyone even
	// if this delete fails — the tombstone is authoritative and the sweeper
	// collects it at expiry. Best-effort from here on.
	st.deleteStoredCode(ctx, code)
	stored.Consumed = false
	return stored, true, nil
}

// deleteStoredCode removes a redeemed code's object. Only ever called once the
// caller holds the claim, so a failure is cosmetic: the stored record already
// says Consumed and the sweeper will collect it.
func (st *mcpOAuthState) deleteStoredCode(ctx context.Context, code string) {
	err := st.store.DeleteRaw(ctx, st.codeKey(code))
	if err != nil {
		slog.Warn("mcp.oauth.code_delete_failed", "err", err)
	}
}

// sweepStoredCodes drops expired code objects. Redemption deletes its own
// object, so this only cleans up after abandoned connects — the stored twin of
// the in-memory sweep above, and the reason persisting codes doesn't trade a
// memory leak for a bucket leak. Runs on the authorize path, which is rare and
// already doing a write; the listing only ever holds codes from the last ten
// minutes plus abandoned ones, which this is in the business of removing.
func (st *mcpOAuthState) sweepStoredCodes(ctx context.Context) {
	keys, err := st.store.ListPrefix(ctx, st.codesPrefix())
	if err != nil {
		slog.Warn("mcp.oauth.code_sweep_failed", "err", err)
		return
	}
	now := time.Now()
	for _, key := range keys {
		obj, err := st.store.ReadRaw(ctx, key)
		if err != nil || obj.Content == "" {
			continue
		}
		stored := mcpAuthCode{}
		// An unparseable record is junk we can never honour, so it goes too.
		parseErr := json.Unmarshal([]byte(obj.Content), &stored)
		if parseErr == nil && !now.After(stored.Expires) {
			continue
		}
		err = st.store.DeleteRaw(ctx, key)
		if err != nil {
			slog.Warn("mcp.oauth.code_sweep_delete_failed", "key", key, "err", err)
		}
	}
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
	clientID, err := s.mcpOAuth.registerClient(c.Request().Context(), req.RedirectURIs, req.ClientName)
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

	code, err := s.mcpOAuth.newCode(c.Request().Context(), email, clientID, redirectURI, challenge)
	if err != nil {
		// Failing here is better than redirecting with a code the token
		// endpoint can never resolve: the user sees the problem now instead of
		// an opaque invalid_grant from their tool.
		return mcpRespString(c, http.StatusInternalServerError, "could not issue authorization code")
	}
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
	ac, ok, err := s.mcpOAuth.takeCode(c.Request().Context(), c.FormValue("code"))
	if err != nil {
		// The store failed, so we never learned whether the code was good.
		// Saying invalid_grant would be a terminal verdict the client acts on
		// by restarting the whole flow; temporarily_unavailable tells it to
		// retry, which is what actually recovers.
		slog.Warn("mcp.oauth.take_code_failed", "err", err)
		return mcpRespJSON(c, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
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
