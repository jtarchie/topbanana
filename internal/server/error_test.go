package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestErrorCopyForStatus guards the per-status-family copy on the error
// page. Catches the kind of drift the impeccable critique surfaced (a
// monkey-themed tagline applied uniformly to every status code, regardless
// of family).
func TestErrorCopyForStatus(t *testing.T) {
	cases := []struct {
		name             string
		code             int
		wantTitleContain string
		bannedInTagline  []string
	}{
		{"401 says sign in", http.StatusUnauthorized, "Sign in", []string{"banana", "peel", "vine"}},
		{"403 says no access", http.StatusForbidden, "access", []string{"vine"}},
		{"404 keeps the banana joke", http.StatusNotFound, "banana", []string{"vine"}},
		{"500 lands sober", http.StatusInternalServerError, "peel", []string{"banana"}},
		{"502 also 5xx-shaped", http.StatusBadGateway, "peel", []string{"vine"}},
		{"418 (rare 4xx) hits the generic 4xx branch", http.StatusTeapot, "process", []string{"vine"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, tagline := errorCopyForStatus(tc.code)
			if title == "" || tagline == "" {
				t.Fatalf("empty copy for %d: title=%q tagline=%q", tc.code, title, tagline)
			}
			if !strings.Contains(strings.ToLower(title), strings.ToLower(tc.wantTitleContain)) {
				t.Errorf("title for %d = %q; want it to contain %q", tc.code, title, tc.wantTitleContain)
			}
			for _, banned := range tc.bannedInTagline {
				if strings.Contains(strings.ToLower(tagline), strings.ToLower(banned)) {
					t.Errorf("tagline for %d = %q; should not contain %q (cross-family leakage)", tc.code, tagline, banned)
				}
			}
		})
	}
}
