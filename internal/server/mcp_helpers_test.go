package server

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jtarchie/topbanana/auth"
)

// TestMCPTruncate_StaysValidUTF8 is the case a naive s[:n] gets wrong: the cap
// is a byte count, so clipping a multi-byte character mid-sequence would store
// a broken rune in the object's metadata header.
func TestMCPTruncate_StaysValidUTF8(t *testing.T) {
	t.Parallel()

	if got := mcpTruncate("short", 125); got != "short" {
		t.Errorf("under the cap should pass through, got %q", got)
	}
	// "é" is two bytes: a cap of 3 lands inside the second one.
	if got := mcpTruncate("aé", 2); got != "a" {
		t.Errorf("mcpTruncate(\"aé\", 2) = %q; want %q", got, "a")
	}
	long := strings.Repeat("→", 60) // 3 bytes each
	got := mcpTruncate(long, 125)
	if len(got) > 125 {
		t.Errorf("result is %d bytes; want <= 125", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
}

func TestMCPAssetPath(t *testing.T) {
	t.Parallel()

	// A slice, not a map: one case deliberately carries surrounding
	// whitespace, which is exactly what an agent pasting a path sends.
	ok := []struct{ in, want string }{
		{"hero.png", "assets/hero.png"},
		{"assets/hero.png", "assets/hero.png"},
		{"/assets/hero.png", "assets/hero.png"},
		{" hero.png ", "assets/hero.png"},
	}
	for _, tc := range ok {
		in, want := tc.in, tc.want
		got, err := mcpAssetPath(in)
		if err != nil {
			t.Errorf("mcpAssetPath(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("mcpAssetPath(%q) = %q; want %q", in, got, want)
		}
	}
	for _, bad := range []string{"", "   ", "../secrets.txt", "assets/../../etc/passwd"} {
		_, err := mcpAssetPath(bad)
		if err == nil {
			t.Errorf("mcpAssetPath(%q) should have failed", bad)
		}
	}
}

func TestMCPInviteRole(t *testing.T) {
	t.Parallel()

	got, err := mcpInviteRole("")
	if err != nil || got != auth.RoleAdmin {
		t.Errorf("empty role = (%q, %v); want admin", got, err)
	}
	got, err = mcpInviteRole("super_admin")
	if err != nil || got != auth.RoleSuperAdmin {
		t.Errorf("super_admin = (%q, %v)", got, err)
	}
	// The whole point of not reusing templates.Get's fallback style: a typo
	// must not quietly issue a weaker invite than the operator asked for.
	_, err = mcpInviteRole("superadmin")
	if err == nil {
		t.Error("misspelled role should be rejected, not downgraded")
	}
}

func TestMCPInviteTiers(t *testing.T) {
	t.Parallel()

	got, err := mcpInviteTiers(nil)
	if err != nil || len(got) != 0 {
		t.Errorf("nil map = (%v, %v); want an empty map", got, err)
	}
	// An all-blank map contributes nothing, so quotas.Encode leaves no field
	// behind on the invite record.
	got, err = mcpInviteTiers(map[string]string{"author": "  "})
	if err != nil || len(got) != 0 {
		t.Errorf("blank values = (%v, %v); want an empty map", got, err)
	}
	got, err = mcpInviteTiers(map[string]string{"author": "anthropic/claude-opus-4-7"})
	if err != nil || got["author"] != "anthropic/claude-opus-4-7" {
		t.Errorf("valid tier = (%v, %v)", got, err)
	}
	_, err = mcpInviteTiers(map[string]string{"writer": "x"})
	if err == nil {
		t.Error("unknown tier key should be rejected")
	}
}

func TestMCPTemplate(t *testing.T) {
	t.Parallel()

	blank, err := mcpTemplate("")
	if err != nil || blank == nil {
		t.Fatalf("empty id = (%v, %v); want the default template", blank, err)
	}
	known, err := mcpTemplate("restaurant")
	if err != nil || known.ID != "restaurant" {
		t.Errorf("restaurant = (%v, %v)", known, err)
	}
	_, err = mcpTemplate("restraunt")
	if err == nil {
		t.Fatal("unknown template should error rather than fall back to blank")
	}
	// The error lists what is available, so an agent can correct itself
	// without another round trip.
	if !strings.Contains(err.Error(), "restaurant") {
		t.Errorf("error should list valid ids, got: %v", err)
	}
}
