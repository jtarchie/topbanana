package server_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestLegalPages_PublicAccess confirms /privacy and /terms render without a
// session cookie and contain the data-ownership messaging and contact email.
// Both pages must be reachable before signup, so requireUser would be the
// wrong gate.
func TestLegalPages_PublicAccess(t *testing.T) {
	st := minioStore(t)

	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	currentYear := strconv.Itoa(time.Now().Year())

	cases := []struct {
		path      string
		wantTitle string
		crossLink string
		mustHave  []string
	}{
		{
			path:      "/privacy",
			wantTitle: "Privacy Policy",
			crossLink: "/terms",
			mustHave:  []string{"You own your data", "hello@topbanana.dev"},
		},
		{
			path:      "/terms",
			wantTitle: "Terms of Service",
			crossLink: "/privacy",
			mustHave:  []string{"You own your data", "hello@topbanana.dev"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodGet, httpSrv.URL+tc.path, nil)
			if err != nil {
				t.Fatalf("new GET %s: %v", tc.path, err)
			}
			req.Host = "localhost"
			// Deliberately no cookie attached — legal pages must work for
			// non-logged-in visitors.
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			defer func() { _ = resp.Body.Close() }()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status %s: got %d want 200", tc.path, resp.StatusCode)
			}

			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read body %s: %v", tc.path, err)
			}
			html := string(body)

			if !strings.Contains(html, tc.wantTitle) {
				t.Errorf("%s body missing title %q", tc.path, tc.wantTitle)
			}
			for _, want := range tc.mustHave {
				if !strings.Contains(html, want) {
					t.Errorf("%s body missing %q", tc.path, want)
				}
			}
			if !strings.Contains(html, `href="`+tc.crossLink+`"`) {
				t.Errorf("%s body missing cross-link to %s", tc.path, tc.crossLink)
			}
			// Footer should carry the dynamic copyright year from Chrome.
			if !strings.Contains(html, currentYear+" Top Banana") {
				t.Errorf("%s footer missing %q (copyright year)", tc.path, currentYear+" Top Banana")
			}
		})
	}
}

// TestLegalFooterOnLandingPage confirms the new footer (with Privacy/Terms
// links + dynamic year) renders on a chrome'd page that already embeds Chrome.
// Catches regressions where the layout's `{{ if .Year }}` guard would otherwise
// quietly fall back to the hardcoded 2026.
func TestLegalFooterOnLandingPage(t *testing.T) {
	st := minioStore(t)

	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
	if err != nil {
		t.Fatalf("new GET /: %v", err)
	}
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status /: got %d want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /: %v", err)
	}
	html := string(body)

	currentYear := strconv.Itoa(time.Now().Year())
	wants := []string{
		currentYear + " Top Banana",
		`href="/privacy"`,
		`href="/terms"`,
		"Your data is yours",
	}
	for _, w := range wants {
		if !strings.Contains(html, w) {
			t.Errorf("landing footer missing %q", w)
		}
	}

	// Confirm the hardcoded 2026 fallback path is NOT taken when Chrome is
	// populated — i.e. Chrome's Year injection actually runs.
	if currentYear != "2026" && strings.Contains(html, "2026 Top Banana") {
		t.Errorf("footer still rendering hardcoded 2026 fallback even with Chrome populated")
	}
}
