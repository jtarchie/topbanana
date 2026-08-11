// Package oauth is a minimal OAuth 2.1 authorization server for fronting an
// MCP endpoint. An MCP client (Claude Code) discovers it via the well-known
// metadata, dynamically registers, then runs the authorization-code + PKCE
// flow. Tokens are the JWTs minted by the parent auth package.
//
// It does not own human authentication. Config.CurrentUser hands that back to
// whatever session system the host application already has, so adopting this
// introduces no second login: /oauth/authorize issues a code only once
// CurrentUser reports a signed-in browser.
//
// Both halves of the flow's state are persisted through blob.Blobs, because
// both outlive a single request and neither survives being process-local:
//
//   - Client registrations. An MCP client registers once and reuses its
//     client_id indefinitely, so a map meant every restart or redeploy broke
//     returning clients with "unknown client_id".
//   - Authorization codes, so the /oauth/token POST can land on a different
//     instance than the /oauth/authorize that issued it. Without this a
//     two-instance deployment fails roughly half of all connects, and retrying
//     is a coin flip rather than a fix. There is deliberately no in-memory
//     copy — see newCode.
//
// Single use is enforced by a compare-and-set on the stored code, not by the
// delete that follows it — see takeCode. Two redemptions racing on the same
// code both read the same version and exactly one write can win; the loser is
// refused. Read-then-delete would give both a token, since neither can tell it
// lost.
package oauth

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
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/auth/internal/ratelimit"
)

const mcpAuthCodeTTL = 10 * time.Minute

// refreshTTL bounds a rotation chain; refreshTombstoneGrace is how long a
// consumed record is kept past expiry so a replay is still detectable rather
// than just missing.
const (
	refreshTTL            = auth.MCPRefreshTTL
	refreshTombstoneGrace = 7 * 24 * time.Hour
)

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
	store blob.Blobs

	// prefix is the bucket namespace this instance owns; see mcpOAuthPrefix.
	prefix string

	// registerLimiter throttles /oauth/register per client IP. RFC 7591
	// registration is necessarily unauthenticated — an MCP client registers
	// before any human signs in — so without this an anonymous loop writes
	// stored objects without bound. Nothing is exposed by the spam (a
	// client_id is inert until /oauth/authorize sees a signed-in session);
	// this caps the junk and the storage bill.
	registerLimiter *ratelimit.Limiter
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

// Config wires the authorization server. Blobs, Secret, and CurrentUser are
// required; everything else has a working default.
type Config struct {
	// Blobs stores registrations and pending codes.
	Blobs blob.Blobs

	// BaseURL is the externally-reachable origin the metadata advertises. It
	// must match what clients actually dial, because the bearer middleware
	// pins the resource-metadata URL against it.
	BaseURL string

	// Secret signs the bearer tokens minted at the token endpoint. The same
	// secret must be given to auth.MCPTokenVerifier on the protected endpoint.
	Secret string

	// CurrentUser reports the signed-in user's email for the browser making
	// this request. This is the human-authentication step: the authorization
	// endpoint refuses to issue a code without one, and bounces to LoginPath
	// instead. Delegating it is what keeps this package from introducing a
	// second login system.
	CurrentUser func(c *echo.Context) (string, bool)

	// LoginPath is where an unauthenticated browser is sent, with the original
	// URL in a `return` query parameter. Defaults to /login.
	LoginPath string

	// Prefix namespaces stored registrations and codes. Defaults to
	// _auth/oauth/. Give separate instances separate prefixes; the listing
	// paths treat everything under it as theirs.
	Prefix string
}

// Server is the authorization server. Construct with New, publish with Mount.
type Server struct {
	st  *mcpOAuthState
	cfg Config
}

// New validates cfg and returns the authorization server.
func New(cfg Config) (*Server, error) {
	switch {
	case cfg.Blobs == nil:
		return nil, errors.New("oauth: Blobs required")
	case cfg.Secret == "":
		return nil, errors.New("oauth: Secret required")
	case cfg.CurrentUser == nil:
		return nil, errors.New("oauth: CurrentUser required — this package does not authenticate humans itself")
	case cfg.BaseURL == "":
		return nil, errors.New("oauth: BaseURL required")
	}
	if cfg.LoginPath == "" {
		cfg.LoginPath = "/login"
	}
	if cfg.Prefix == "" {
		cfg.Prefix = mcpOAuthPrefix
	}
	return &Server{st: newState(cfg.Blobs, cfg.Prefix), cfg: cfg}, nil
}

func newState(b blob.Blobs, prefix string) *mcpOAuthState {
	return &mcpOAuthState{
		store:  b,
		prefix: prefix,
		// Registering is a once-per-tool-install event, so this is far above
		// any legitimate rate while still blunting a script: ~1 per 5s
		// sustained per IP, burst 5 for a shared NAT.
		registerLimiter: ratelimit.New(0.2, 5),
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
		return "", fmt.Errorf("oauth: marshal oauth client: %w", err)
	}
	err = st.store.Put(ctx, st.clientKey(id), string(body))
	if err != nil {
		return "", fmt.Errorf("oauth: write oauth client: %w", err)
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
	obj, err := st.store.Get(ctx, st.clientKey(id))
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("oauth: read oauth client: %w", err)
	}
	if obj.Content == "" {
		return mcpOAuthClient{}, false, nil
	}
	c := mcpOAuthClient{}
	err = json.Unmarshal([]byte(obj.Content), &c)
	if err != nil {
		return mcpOAuthClient{}, false, fmt.Errorf("oauth: parse oauth client: %w", err)
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
	keys, err := st.store.List(ctx, st.clientsPrefix())
	if err != nil {
		return nil, fmt.Errorf("oauth: list oauth clients: %w", err)
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
		obj, err := st.store.Get(ctx, key)
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

// countClients reports how many registrations exist, for the diagnostic log on
// the unknown-client path. Returns -1 rather than an error: it is telemetry on
// a path that is already failing, and must never become a second failure.
func (st *mcpOAuthState) countClients(ctx context.Context) int {
	keys, err := st.store.List(ctx, st.clientsPrefix())
	if err != nil {
		return -1
	}
	return len(keys)
}

// revokeClient deletes a registration. Because client() reads through to the
// store on every authorize, the delete takes effect immediately and on every
// instance. Returns ErrClientNotFound for an id that was never a valid
// registration, so the console doesn't report a revocation that didn't happen.
func (st *mcpOAuthState) revokeClient(ctx context.Context, id string) error {
	if !mcpTokenPattern.MatchString(id) {
		return ErrClientNotFound
	}
	err := st.store.Delete(ctx, st.clientKey(id))
	if err != nil {
		return fmt.Errorf("oauth: revoke oauth client: %w", err)
	}
	// Without this the registration is gone but the client keeps renewing off
	// a live rotation chain, so "revoked" in the console would not be true.
	st.revokeClientRefresh(ctx, id)
	return nil
}

// ErrClientNotFound is returned when a revoke targets an id that could never
// name a registration.
var ErrClientNotFound = errors.New("oauth: client not found")

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
		return "", fmt.Errorf("oauth: marshal auth code: %w", err)
	}
	err = st.store.Put(ctx, st.codeKey(code), string(body))
	if err != nil {
		return "", fmt.Errorf("oauth: write auth code: %w", err)
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
	obj, err := st.store.Get(ctx, st.codeKey(code))
	if err != nil {
		return mcpAuthCode{}, false, fmt.Errorf("oauth: read auth code: %w", err)
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
		return mcpAuthCode{}, false, fmt.Errorf("oauth: marshal consumed auth code: %w", err)
	}
	err = st.store.PutIfMatch(ctx, st.codeKey(code), string(consumed), obj.ETag)
	if errors.Is(err, blob.ErrPrecondition) {
		// Someone else claimed it first, or it was swept out from under us.
		return mcpAuthCode{}, false, nil
	}
	if err != nil {
		return mcpAuthCode{}, false, fmt.Errorf("oauth: claim auth code: %w", err)
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
	err := st.store.Delete(ctx, st.codeKey(code))
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
	keys, err := st.store.List(ctx, st.codesPrefix())
	if err != nil {
		slog.Warn("mcp.oauth.code_sweep_failed", "err", err)
		return
	}
	now := time.Now()
	for _, key := range keys {
		obj, err := st.store.Get(ctx, key)
		if err != nil || obj.Content == "" {
			continue
		}
		stored := mcpAuthCode{}
		// An unparseable record is junk we can never honour, so it goes too.
		parseErr := json.Unmarshal([]byte(obj.Content), &stored)
		if parseErr == nil && !now.After(stored.Expires) {
			continue
		}
		err = st.store.Delete(ctx, key)
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

// Mount publishes the metadata and OAuth endpoints on e. The protected
// resource itself (/mcp) is the host application's to mount — this package
// issues the tokens, it does not serve the tools. Pair it with
// auth.MCPTokenVerifier over the same Secret, and advertise
// BaseURL + "/.well-known/oauth-protected-resource" as the resource metadata
// URL so discovery points back here.
func (s *Server) Mount(e *echo.Echo) {
	e.GET("/.well-known/oauth-protected-resource", s.protectedResourceHandler)
	e.GET("/.well-known/oauth-authorization-server", s.authServerMetadataHandler)
	e.POST("/oauth/register", s.registerHandler)
	e.GET("/oauth/authorize", s.authorizeHandler)
	e.POST("/oauth/token", s.tokenHandler)
}

// ResourceMetadataURL is what the bearer middleware on the protected endpoint
// should advertise, so a 401 tells the client where to authorize.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.BaseURL + "/.well-known/oauth-protected-resource"
}

// Client is one registration, as the admin surface sees it.
type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
	Created      time.Time
}

// Clients lists every registration, newest first. O(N) reads over the prefix;
// fine at the scale this runs at (one record per tool install).
func (s *Server) Clients(ctx context.Context) ([]Client, error) {
	stored, err := s.st.listClients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Client, 0, len(stored))
	for _, c := range stored {
		out = append(out, Client{
			ID:           c.ID,
			Name:         c.ClientName,
			RedirectURIs: c.RedirectURIs,
			Created:      c.Created,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out, nil
}

// RevokeClient deletes a registration. Because the authorize path reads
// through to the store on every request, this takes effect immediately and on
// every instance. Returns ErrClientNotFound for an id that could never name a
// registration, so a caller doesn't report a revocation that didn't happen.
func (s *Server) RevokeClient(ctx context.Context, id string) error {
	return s.st.revokeClient(ctx, id)
}

// --- well-known metadata ----------------------------------------------------

func (s *Server) protectedResourceHandler(c *echo.Context) error {
	base := s.cfg.BaseURL
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"resource":                 base + "/mcp",
		"authorization_servers":    []string{base},
		"scopes_supported":         []string{auth.MCPScope},
		"bearer_methods_supported": []string{"header"},
	})
}

func (s *Server) authServerMetadataHandler(c *echo.Context) error {
	base := s.cfg.BaseURL
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"issuer":                                base,
		"authorization_endpoint":                base + "/oauth/authorize",
		"token_endpoint":                        base + "/oauth/token",
		"registration_endpoint":                 base + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"scopes_supported":                      []string{auth.MCPScope},
	})
}

// --- dynamic client registration (RFC 7591, minimal) ------------------------

func (s *Server) registerHandler(c *echo.Context) error {
	if !s.st.registerLimiter.Allow(c.RealIP()) {
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
	clientID, err := s.st.registerClient(c.Request().Context(), req.RedirectURIs, req.ClientName)
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

func (s *Server) authorizeHandler(c *echo.Context) error {
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
	client, ok, err := s.st.client(c.Request().Context(), clientID)
	if err != nil {
		slog.Warn("mcp.oauth.client_lookup_failed", "client_id", clientID, "err", err)
		return mcpRespString(c, http.StatusInternalServerError, "client lookup failed")
	}
	if !ok {
		// Logged because this is the one failure an operator cannot diagnose
		// from the outside: the browser shows "unknown client_id" and nothing
		// says which id, or whether any registration exists at all. A client
		// that registered before registrations were persisted caches its id
		// forever and presents a dead one on every attempt — indistinguishable
		// from a typo without this line. client_id is a public identifier, not
		// a credential, so it is safe to log.
		slog.Warn("mcp.oauth.unknown_client",
			"client_id", clientID,
			"registered_clients", s.st.countClients(c.Request().Context()),
			"hint", "if this is nonzero but yours is missing, the client cached an id from before registrations were stored; re-add the MCP server to force it to register again")
		return mcpRespString(c, http.StatusBadRequest, "unknown client_id")
	}
	redirectURI := q.Get("redirect_uri")
	if !client.allows(redirectURI) {
		return mcpRespString(c, http.StatusBadRequest, "redirect_uri not registered for this client")
	}

	// Reuse the passkey session for human authentication. If the browser
	// isn't signed in, bounce to /login; the user signs in and re-initiates
	// the connection (their session cookie then satisfies this check).
	email, ok := s.cfg.CurrentUser(c)
	if !ok {
		return mcpRedirect(c, s.cfg.LoginPath+"?return="+url.QueryEscape(c.Request().URL.String()))
	}

	code, err := s.st.newCode(c.Request().Context(), email, clientID, redirectURI, challenge)
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

func (s *Server) tokenHandler(c *echo.Context) error {
	switch c.FormValue("grant_type") {
	case "authorization_code":
		return s.exchangeCode(c)
	case "refresh_token":
		return s.exchangeRefresh(c)
	default:
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
	}
}

func (s *Server) exchangeCode(c *echo.Context) error {
	ctx := c.Request().Context()
	ac, ok, err := s.st.takeCode(ctx, c.FormValue("code"))
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
	clientID := c.FormValue("client_id")
	if ac.ClientID != clientID || ac.RedirectURI != c.FormValue("redirect_uri") {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}
	if !verifyPKCE(c.FormValue("code_verifier"), ac.CodeChallenge) {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{
			"error":             "invalid_grant",
			"error_description": "PKCE verification failed",
		})
	}
	return s.grant(c, ac.Email, clientID, "")
}

func (s *Server) exchangeRefresh(c *echo.Context) error {
	ctx := c.Request().Context()
	clientID := c.FormValue("client_id")
	rec, ok, err := s.st.redeemRefresh(ctx, c.FormValue("refresh_token"), clientID)

	if errors.Is(err, errRefreshReused) {
		// The token was already rotated away, so at least two parties hold it.
		// Kill the chain rather than this link: the legitimate client is
		// forced to authorize again, which is the right outcome once a
		// credential is known to have escaped.
		slog.Warn("mcp.oauth.refresh_reuse_detected",
			"client_id", clientID, "family", rec.Family,
			"action", "revoked the whole refresh chain")
		s.st.revokeFamily(ctx, rec.Family)
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{
			"error":             "invalid_grant",
			"error_description": "refresh token reuse detected; the session was revoked",
		})
	}
	if err != nil {
		slog.Warn("mcp.oauth.redeem_refresh_failed", "err", err)
		return mcpRespJSON(c, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	if !ok {
		return mcpRespJSON(c, http.StatusBadRequest, map[string]string{"error": "invalid_grant"})
	}
	return s.grant(c, rec.Email, clientID, rec.Family)
}

// grant mints an access token plus the refresh token that renews it. family
// continues an existing rotation chain, or starts one when empty.
//
// A failure to issue the refresh token fails the whole exchange rather than
// returning the access token alone: a client that gets one without the other
// looks connected for twelve hours and then needs a human, which is the exact
// failure this is meant to end.
func (s *Server) grant(c *echo.Context, email, clientID, family string) error {
	token, err := auth.MintMCPToken(s.cfg.Secret, email, auth.MCPTokenTTL)
	if err != nil {
		return mcpRespJSON(c, http.StatusInternalServerError, map[string]string{"error": "server_error"})
	}
	refresh, err := s.st.issueRefresh(c.Request().Context(), email, clientID, family)
	if err != nil {
		slog.Warn("mcp.oauth.issue_refresh_failed", "err", err)
		return mcpRespJSON(c, http.StatusServiceUnavailable, map[string]string{"error": "temporarily_unavailable"})
	}
	s.st.sweepRefresh(c.Request().Context())
	return mcpRespJSON(c, http.StatusOK, map[string]any{
		"access_token":  token,
		"token_type":    "Bearer",
		"expires_in":    int(auth.MCPTokenTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         auth.MCPScope,
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
