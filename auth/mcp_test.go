package auth

import (
	"context"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const testMCPSecret = "test-mcp-secret-please-ignore"

func TestMintAndVerifyMCPToken_RoundTrip(t *testing.T) {
	token, err := MintMCPToken(testMCPSecret, "  User@Example.com ", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	info, err := MCPTokenVerifier(testMCPSecret)(context.Background(), token, nil)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if info.UserID != "user@example.com" {
		t.Fatalf("UserID = %q, want normalized %q", info.UserID, "user@example.com")
	}
	found := false
	for _, s := range info.Scopes {
		if s == MCPScope {
			found = true
		}
	}
	if !found {
		t.Fatalf("scopes %v missing %q", info.Scopes, MCPScope)
	}
}

func TestMintMCPToken_RequiresSecretAndEmail(t *testing.T) {
	_, err := MintMCPToken("", "a@b.com", time.Hour)
	if err == nil {
		t.Fatal("expected error for empty secret")
	}
	_, err = MintMCPToken(testMCPSecret, "   ", time.Hour)
	if err == nil {
		t.Fatal("expected error for empty email")
	}
}

func TestMCPTokenVerifier_RejectsExpired(t *testing.T) {
	token, err := MintMCPToken(testMCPSecret, "a@b.com", -time.Minute)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = MCPTokenVerifier(testMCPSecret)(context.Background(), token, nil)
	if err == nil {
		t.Fatal("expected expired token to be rejected")
	}
}

func TestMCPTokenVerifier_RejectsBadSignature(t *testing.T) {
	token, err := MintMCPToken(testMCPSecret, "a@b.com", time.Hour)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = MCPTokenVerifier("a-different-secret")(context.Background(), token, nil)
	if err == nil {
		t.Fatal("expected token signed with another secret to be rejected")
	}
}

func TestMCPTokenVerifier_RejectsWrongAudience(t *testing.T) {
	// Sign a token with the right secret but the wrong audience; the verifier
	// must reject it so an API-surface token can't be replayed against MCP.
	claims := mcpClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   "a@b.com",
			Audience:  jwt.ClaimStrings{"api"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(testMCPSecret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	_, err = MCPTokenVerifier(testMCPSecret)(context.Background(), signed, nil)
	if err == nil {
		t.Fatal("expected wrong-audience token to be rejected")
	}
}
