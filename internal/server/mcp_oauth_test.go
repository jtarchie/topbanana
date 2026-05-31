package server

import (
	"crypto/sha256"
	"encoding/base64"
	"testing"
	"time"
)

func TestVerifyPKCE(t *testing.T) {
	verifier := "the-quick-brown-fox-jumps-over-the-lazy-dog-0123456789"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])

	if !verifyPKCE(verifier, challenge) {
		t.Fatal("matching verifier/challenge should pass")
	}
	if verifyPKCE("wrong-verifier", challenge) {
		t.Fatal("mismatched verifier should fail")
	}
	if verifyPKCE("", challenge) {
		t.Fatal("empty verifier should fail")
	}
	if verifyPKCE(verifier, "") {
		t.Fatal("empty challenge should fail")
	}
}

func TestMCPOAuthState_CodeSingleUse(t *testing.T) {
	st := newMCPOAuthState()
	code := st.newCode("user@example.com", "client-1", "https://cb", "challenge")

	ac, ok := st.takeCode(code)
	if !ok {
		t.Fatal("first takeCode should succeed")
	}
	if ac.Email != "user@example.com" || ac.ClientID != "client-1" || ac.RedirectURI != "https://cb" {
		t.Fatalf("code payload mismatch: %+v", ac)
	}
	if _, ok := st.takeCode(code); ok {
		t.Fatal("second takeCode should fail (single use)")
	}
}

func TestMCPOAuthState_CodeExpiry(t *testing.T) {
	st := newMCPOAuthState()
	st.codes["expired"] = mcpAuthCode{
		Email:   "user@example.com",
		Expires: time.Now().Add(-time.Minute),
	}
	if _, ok := st.takeCode("expired"); ok {
		t.Fatal("expired code should not be honoured")
	}
}

func TestMCPOAuthState_ClientRegistration(t *testing.T) {
	st := newMCPOAuthState()
	id := st.registerClient([]string{"https://cb/one", "https://cb/two"})
	if id == "" {
		t.Fatal("registerClient should return a non-empty id")
	}

	client, ok := st.client(id)
	if !ok {
		t.Fatal("registered client should be retrievable")
	}
	if !client.allows("https://cb/one") || !client.allows("https://cb/two") {
		t.Fatal("registered redirect URIs should be allowed")
	}
	if client.allows("https://evil/cb") {
		t.Fatal("unregistered redirect URI must not be allowed")
	}
	if _, ok := st.client("nope"); ok {
		t.Fatal("unknown client id should not resolve")
	}
}
