package auth

import (
	"testing"
	"time"
)

const testSecret = "test-mcp-secret-0123456789"

func TestUploadTicketRoundTrip(t *testing.T) {
	t.Parallel()
	tok, err := MintUploadTicket(testSecret, "User@Example.com", "fast-flame-71", UploadTicketTTL, 5<<20)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	email, slug, maxBytes, err := ParseUploadTicket(testSecret, tok)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if email != "user@example.com" { // NormalizeEmail lowercases
		t.Errorf("email = %q, want normalized", email)
	}
	if slug != "fast-flame-71" {
		t.Errorf("slug = %q", slug)
	}
	if maxBytes != 5<<20 {
		t.Errorf("maxBytes = %d", maxBytes)
	}
}

func TestMintUploadTicketValidation(t *testing.T) {
	t.Parallel()
	_, err := MintUploadTicket("", "a@b.com", "slug", UploadTicketTTL, 1)
	if err == nil {
		t.Error("empty secret must error")
	}
	_, err = MintUploadTicket(testSecret, "", "slug", UploadTicketTTL, 1)
	if err == nil {
		t.Error("empty email must error")
	}
	_, err = MintUploadTicket(testSecret, "a@b.com", "", UploadTicketTTL, 1)
	if err == nil {
		t.Error("empty slug must error")
	}
}

func TestParseUploadTicketRejections(t *testing.T) {
	t.Parallel()
	good, err := MintUploadTicket(testSecret, "a@b.com", "slug", UploadTicketTTL, 1)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	_, _, _, err = ParseUploadTicket("a-different-secret", good)
	if err == nil {
		t.Error("wrong secret must be rejected")
	}
	_, _, _, err = ParseUploadTicket(testSecret, good+"x")
	if err == nil {
		t.Error("tampered token must be rejected")
	}

	expired, err := MintUploadTicket(testSecret, "a@b.com", "slug", -time.Minute, 1)
	if err != nil {
		t.Fatalf("mint expired: %v", err)
	}
	_, _, _, err = ParseUploadTicket(testSecret, expired)
	if err == nil {
		t.Error("expired ticket must be rejected")
	}
}

// TestUploadTicketAudienceSeparation is the load-bearing security property: an
// MCP bearer token must not validate as an upload ticket, and vice versa, even
// though both are signed with the same secret.
func TestUploadTicketAudienceSeparation(t *testing.T) {
	t.Parallel()
	mcpTok, err := MintMCPToken(testSecret, "a@b.com", MCPTokenTTL)
	if err != nil {
		t.Fatalf("mint mcp: %v", err)
	}
	_, _, _, err = ParseUploadTicket(testSecret, mcpTok)
	if err == nil {
		t.Error("an MCP bearer token must not parse as an upload ticket")
	}

	ticket, err := MintUploadTicket(testSecret, "a@b.com", "slug", UploadTicketTTL, 1)
	if err != nil {
		t.Fatalf("mint ticket: %v", err)
	}
	verify := MCPTokenVerifier(testSecret)
	_, verr := verify(t.Context(), ticket, nil)
	if verr == nil {
		t.Error("an upload ticket must not pass the MCP bearer verifier")
	}
}
