package server

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSiteSummaryJSON_Domains pins the MCP contract added so clients can map a
// slug to its public hostname(s) without guessing: list_sites/get_site expose
// the site's custom domains under "domains", and omit the key when there are
// none (so blank sites don't carry an empty array).
func TestSiteSummaryJSON_Domains(t *testing.T) {
	t.Parallel()

	withDomains, err := json.Marshal(siteSummary{
		Slug:    "fast-flame-71",
		Domains: []string{"topbanana.dev"},
		URL:     "https://fast-flame-71.apps.topbanana.dev",
	})
	if err != nil {
		t.Fatalf("marshal with domains: %v", err)
	}
	if !strings.Contains(string(withDomains), `"domains":["topbanana.dev"]`) {
		t.Errorf("expected domains in JSON, got %s", withDomains)
	}

	none, err := json.Marshal(siteSummary{
		Slug: "blank-1",
		URL:  "https://blank-1.apps.topbanana.dev",
	})
	if err != nil {
		t.Fatalf("marshal without domains: %v", err)
	}
	if strings.Contains(string(none), "domains") {
		t.Errorf("empty domains must be omitted, got %s", none)
	}
}

// TestMCPContentType locks in the behavior the write_file docs now promise:
// the served content type is inferred from the extension, so non-HTML assets
// (favicons, images) work, while unknown extensions fall back to octet-stream.
func TestMCPContentType(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"index.html":  "text/html; charset=utf-8",
		"about.htm":   "text/html; charset=utf-8",
		"favicon.svg": "image/svg+xml",
		"logo.png":    "image/png",
		"data.bin":    "application/octet-stream",
	}
	for path, want := range cases {
		if got := mcpContentType(path); got != want {
			t.Errorf("mcpContentType(%q) = %q; want %q", path, got, want)
		}
	}
}
