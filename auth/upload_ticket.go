package auth

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Upload tickets are short-lived JWTs the MCP create_upload_ticket tool hands to
// an external agent so it can curl raw binary bytes to Top Banana's own upload
// endpoint — the model never sees the MCP bearer token, so the tool mints a
// fresh, single-purpose token instead. Signed with the same --mcp-secret as the
// MCP bearer tokens but pinned to a different audience, so neither can be
// replayed as the other.
const (
	// uploadTicketAudience is the required `aud`. Distinct from mcpAudience so an
	// MCP bearer token can't be used as an upload ticket or vice versa.
	uploadTicketAudience = "upload-ticket"
	// UploadTicketTTL bounds how long a minted ticket is honoured. Short because
	// create_upload_ticket can mint a fresh one on demand.
	UploadTicketTTL = 15 * time.Minute
)

// uploadTicketClaims is the JWT payload: the owner email (subject), the slug the
// ticket is scoped to, and the per-file byte cap the upload endpoint enforces.
type uploadTicketClaims struct {
	Slug     string `json:"slug"`
	MaxBytes int64  `json:"max_bytes"`
	jwt.RegisteredClaims
}

// MintUploadTicket signs a ticket authorizing email to upload assets to slug
// (each capped at maxBytes) until it expires. secret is the HMAC signing key
// (--mcp-secret); an empty secret is a configuration error because it would
// make every ticket forgeable.
func MintUploadTicket(secret, email, slug string, ttl time.Duration, maxBytes int64) (string, error) {
	if secret == "" {
		return "", errors.New("auth: mcp secret not configured")
	}
	email = NormalizeEmail(email)
	if email == "" {
		return "", errors.New("auth: upload ticket requires a subject email")
	}
	if slug == "" {
		return "", errors.New("auth: upload ticket requires a slug")
	}
	now := time.Now()
	claims := uploadTicketClaims{
		Slug:     slug,
		MaxBytes: maxBytes,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   email,
			Audience:  jwt.ClaimStrings{uploadTicketAudience},
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("auth: sign upload ticket: %w", err)
	}
	return signed, nil
}

// UploadTicket is a validated upload ticket's claims: the owner email, the
// target slug, and the per-file size cap.
type UploadTicket struct {
	Email    string
	Slug     string
	MaxBytes int64
}

// ParseUploadTicket validates a ticket's HMAC signature, audience, and expiry
// and returns the decoded UploadTicket. Any failure collapses to a single
// error so callers never leak why a ticket was rejected.
func ParseUploadTicket(secret, token string) (UploadTicket, error) {
	// Fail closed on an empty secret, symmetric with MintUploadTicket: an empty
	// HMAC key makes every ticket forgeable, so don't let one verify.
	if secret == "" {
		return UploadTicket{}, errors.New("auth: invalid upload ticket")
	}
	var claims uploadTicketClaims
	parsed, perr := jwt.ParseWithClaims(token, &claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method %v", t.Header["alg"])
		}
		return []byte(secret), nil
	}, jwt.WithAudience(uploadTicketAudience))
	if perr != nil || !parsed.Valid {
		return UploadTicket{}, errors.New("auth: invalid upload ticket")
	}
	if claims.Subject == "" || claims.Slug == "" {
		return UploadTicket{}, errors.New("auth: invalid upload ticket")
	}
	return UploadTicket{Email: claims.Subject, Slug: claims.Slug, MaxBytes: claims.MaxBytes}, nil
}
