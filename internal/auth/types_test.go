package auth

import (
	"testing"

	"github.com/go-webauthn/webauthn/webauthn"
)

// TestUser_RemoveCredential covers the three outcomes the self-service "remove
// passkey" path depends on: removing a present credential, leaving the others
// intact, and reporting a miss without mutating the slice.
func TestUser_RemoveCredential(t *testing.T) {
	t.Parallel()

	u := &User{Credentials: []webauthn.Credential{
		{ID: []byte("alpha")},
		{ID: []byte("beta")},
		{ID: []byte("gamma")},
	}}

	if !u.RemoveCredential([]byte("beta")) {
		t.Fatalf("RemoveCredential(beta) = false, want true")
	}
	if len(u.Credentials) != 2 {
		t.Fatalf("after remove: len = %d, want 2", len(u.Credentials))
	}
	for _, c := range u.Credentials {
		if string(c.ID) == "beta" {
			t.Errorf("beta still present after removal")
		}
	}
	// Survivors keep their order.
	if string(u.Credentials[0].ID) != "alpha" || string(u.Credentials[1].ID) != "gamma" {
		t.Errorf("survivors reordered: %q, %q", u.Credentials[0].ID, u.Credentials[1].ID)
	}

	if u.RemoveCredential([]byte("missing")) {
		t.Errorf("RemoveCredential(missing) = true, want false")
	}
	if len(u.Credentials) != 2 {
		t.Errorf("missing removal mutated slice: len = %d, want 2", len(u.Credentials))
	}
}
