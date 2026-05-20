package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/buildabear/internal/build"
)

func TestCheckAPIOrigin_PublicAPIBypass(t *testing.T) {
	t.Parallel()

	// EnablesPublicAPI must short-circuit the origin check so opt-in public
	// endpoints (webhooks, public JSON) are reachable cross-origin.
	r := httptest.NewRequest(http.MethodPost, "http://host.example/api/foo", strings.NewReader("{}"))
	r.Host = "host.example"
	// No Origin / Referer — strictest case.
	err := checkAPIOrigin(r, build.SiteMeta{EnablesPublicAPI: true})
	if err != nil {
		t.Errorf("public-API meta should bypass origin check, got %v", err)
	}
}

func TestCheckAPIOrigin_SafeMethodsBypass(t *testing.T) {
	t.Parallel()

	// GET/HEAD/OPTIONS are idempotent; pages must be able to fetch their own
	// JSON endpoints without the check firing.
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		m := m
		t.Run(m, func(t *testing.T) {
			t.Parallel()
			r := httptest.NewRequest(m, "http://host.example/api/foo", nil)
			r.Host = "host.example"
			err := checkAPIOrigin(r, build.SiteMeta{})
			if err != nil {
				t.Errorf("safe method %s rejected: %v", m, err)
			}
		})
	}
}

func TestCheckAPIOrigin_MatchingOriginAllowed(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "http://host.example/api/foo", strings.NewReader("{}"))
	r.Host = "host.example"
	r.Header.Set("Origin", "http://host.example")
	err := checkAPIOrigin(r, build.SiteMeta{})
	if err != nil {
		t.Errorf("matching Origin rejected: %v", err)
	}
}

func TestCheckAPIOrigin_MatchingRefererAllowed(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "http://host.example/api/foo", strings.NewReader("{}"))
	r.Host = "host.example"
	r.Header.Set("Referer", "http://host.example/page.html")
	err := checkAPIOrigin(r, build.SiteMeta{})
	if err != nil {
		t.Errorf("matching Referer rejected: %v", err)
	}
}

func TestCheckAPIOrigin_CrossOriginBlocked(t *testing.T) {
	t.Parallel()

	r := httptest.NewRequest(http.MethodPost, "http://host.example/api/foo", strings.NewReader("{}"))
	r.Host = "host.example"
	r.Header.Set("Origin", "http://attacker.example")
	err := checkAPIOrigin(r, build.SiteMeta{})
	if err == nil {
		t.Fatal("cross-origin POST was not blocked")
	}
}

func TestCheckAPIOrigin_FailsClosedWhenBothHeadersMissing(t *testing.T) {
	t.Parallel()

	// State-changing POST with neither Origin nor Referer. Modern browsers
	// always send at least one — absent both is a scripted client. Must fail
	// closed (the public-API opt-in is the only escape hatch).
	r := httptest.NewRequest(http.MethodPost, "http://host.example/api/foo", strings.NewReader("{}"))
	r.Host = "host.example"
	err := checkAPIOrigin(r, build.SiteMeta{})
	if err == nil {
		t.Errorf("missing Origin AND Referer should fail closed")
	}
}

func TestApiOriginMatches(t *testing.T) {
	t.Parallel()

	cases := []struct {
		header, host string
		want         bool
	}{
		{"http://host.example", "host.example", true},
		{"https://host.example", "host.example", true},
		{"http://host.example:8080", "host.example", true}, // port-insensitive
		{"http://host.example", "host.example:80", true},   // port-insensitive both ways
		{"http://other.example", "host.example", false},
		{"", "host.example", false},
		{"http://host.example", "", false},
		{"not a url", "host.example", false},
		{"//host.example/path", "host.example", true}, // protocol-relative
	}
	for _, c := range cases {
		c := c
		t.Run(c.header+"|"+c.host, func(t *testing.T) {
			t.Parallel()
			got := apiOriginMatches(c.header, c.host)
			if got != c.want {
				t.Errorf("apiOriginMatches(%q, %q) = %v, want %v", c.header, c.host, got, c.want)
			}
		})
	}
}

func TestIsAPISafeMethod(t *testing.T) {
	t.Parallel()

	cases := map[string]bool{
		http.MethodGet:     true,
		http.MethodHead:    true,
		http.MethodOptions: true,
		"get":              true, // case-insensitive
		http.MethodPost:    false,
		http.MethodPut:     false,
		http.MethodDelete:  false,
		http.MethodPatch:   false,
	}
	for method, want := range cases {
		method, want := method, want
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			if got := isAPISafeMethod(method); got != want {
				t.Errorf("isAPISafeMethod(%q) = %v, want %v", method, got, want)
			}
		})
	}
}

func TestValidateFunctionPathName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		wantErr bool
	}{
		{"submit", false},
		{"send-email", false},
		{"compute_total", false},
		{"a1", false},
		{"", true},
		{"Capital", true}, // uppercase rejected
		{"with space", true},
		{"path/traversal", true},        // slash rejected
		{"dots.allowed?", true},         // dots not in the allowlist
		{strings.Repeat("a", 41), true}, // length cap
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := validateFunctionPathName(c.name)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q", c.name)
				}
				return
			}
			if err != nil {
				t.Errorf("unexpected error for %q: %v", c.name, err)
			}
		})
	}
}
