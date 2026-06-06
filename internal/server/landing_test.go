package server

import (
	"testing"

	"github.com/jtarchie/topbanana/internal/templates"
)

// TestLandingFeaturedIDsExistInRegistry guards against the curated landing
// list silently drifting from the template registry. If someone renames or
// removes a template that's in landingFeaturedIDs, the landing page would
// quietly show fewer than three featured cards instead of failing loudly.
func TestLandingFeaturedIDsExistInRegistry(t *testing.T) {
	byID := make(map[string]bool, len(templates.All()))
	for _, tpl := range templates.All() {
		byID[tpl.ID] = true
	}
	for id := range landingFeaturedIDs {
		if !byID[id] {
			t.Errorf("landingFeaturedIDs contains %q but it's not in templates.All(); the curated list drifted from the registry", id)
		}
	}
	if got, want := len(landingFeaturedIDs), 3; got != want {
		t.Errorf("landingFeaturedIDs size = %d; want %d (the landing layout assumes exactly three featured picks)", got, want)
	}
}
