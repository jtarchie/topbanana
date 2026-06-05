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
