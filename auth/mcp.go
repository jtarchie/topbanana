package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/golang-jwt/jwt/v5"
	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
)

// MCP bearer tokens are short-lived JWTs minted by the OAuth token endpoint
// (see internal/server/mcp_oauth.go) once a user has authenticated through the
// existing passkey login. They carry the user's canonical email in the subject
// and a fixed audience so a token minted for, say, the web API surface can't be
// replayed against the MCP endpoint. This mirrors pocketci's auth/mcp.go.
const (
	// mcpAudience is the required `aud` claim. A token missing it (or carrying
	// a different audience) is rejected by the verifier — audience pinning is
	// the cheap guard against cross-surface token replay.
	mcpAudience = "mcp"
	// MCPScope is the OAuth scope the MCP resource requires. Tokens carry it in
	// the `scope` claim and the bearer middleware checks for it.
	MCPScope = "mcp"
	// MCPTokenTTL bounds how long a minted bearer token is honoured. Short on
	// purpose: the token is a self-contained JWT that nothing can revoke, so
	// its lifetime is the whole of its blast radius. Clients stay connected
	// past it by refreshing, not by holding it longer.
	MCPTokenTTL = 12 * time.Hour

	// MCPRefreshTTL is an idle timeout, not a cap on the chain: every rotation
	// issues a token expiring this far from now, so a client that keeps
	// refreshing never has to authorize in a browser again, and one that goes
	// quiet for this long does.
	//
	// That is the intended behaviour — the point of refresh tokens here is to
	// stop interrupting a working connection — but it means the ceiling on a
	// stolen chain is not this constant. It is however long the theft goes
	// unnoticed, bounded by reuse detection: the moment the victim's client
	// refreshes with a token the attacker already spent (or vice versa), the
	// whole family is revoked. An absolute cap would add a second bound at
	// the cost of a scheduled browser interruption; that trade has not been
	// taken.
	MCPRefreshTTL = 30 * 24 * time.Hour
)

// mcpClaims is the JWT payload. Scopes is a non-standard claim the bearer
// middleware reads; everything else is RFC 7519 registered claims.
type mcpClaims struct {
	Scopes []string `json:"scope,omitempty"`
	jwt.RegisteredClaims
}

// MintMCPToken signs a bearer JWT for email valid for ttl. secret is the HMAC
// signing key (the --mcp-secret flag); an empty secret is a configuration error
// because it would make every token forgeable.
func MintMCPToken(secret, email string, ttl time.Duration) (string, error) {
	if secret == "" {
		return "", errors.New("auth: mcp secret not configured")
	}
	email = NormalizeEmail(email)
	if email == "" {
		return "", errors.New("auth: mcp token requires a subject email")
	}
	now := time.Now()
	claims := mcpClaims{
		Scopes: []string{MCPScope},
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   email,
			Audience:  jwt.ClaimStrings{mcpAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth: sign mcp token: %w", err)
	}
	return signed, nil
}

// MCPTokenVerifier returns the go-sdk TokenVerifier the bearer middleware calls
// per request. It validates the HMAC signature and the audience, then projects
// the claims into the SDK's TokenInfo so handlers can read the caller's email
// off the request context. Any failure collapses to mcpauth.ErrInvalidToken so
// we never leak why a token was rejected.
func MCPTokenVerifier(secret string) mcpauth.TokenVerifier {
	return func(_ context.Context, token string, _ *http.Request) (*mcpauth.TokenInfo, error) {
		// Fail closed on an empty secret, symmetric with MintMCPToken: an empty
		// HMAC key makes every token forgeable. The only caller gates this
		// behind --mcp-secret today, but keeping the guard local means the
		// invariant doesn't depend on a check in another package.
		if secret == "" {
			return nil, mcpauth.ErrInvalidToken
		}
		var claims mcpClaims
		parsed, err := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
			}
			return []byte(secret), nil
		}, jwt.WithAudience(mcpAudience))
		if err != nil || !parsed.Valid {
			return nil, mcpauth.ErrInvalidToken
		}
		if claims.Subject == "" {
			return nil, mcpauth.ErrInvalidToken
		}
		var expiration time.Time
		if claims.ExpiresAt != nil {
			expiration = claims.ExpiresAt.Time
		}
		return &mcpauth.TokenInfo{
			Scopes:     claims.Scopes,
			Expiration: expiration,
			UserID:     claims.Subject,
		}, nil
	}
}
