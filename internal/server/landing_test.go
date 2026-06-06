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
	for _, id := range landingFeaturedIDs {
		if !byID[id] {
			t.Errorf("landingFeaturedIDs contains %q but it's not in templates.All(); the curated list drifted from the registry", id)
		}
	}
	if got, want := len(landingFeaturedIDs), 3; got != want {
		t.Errorf("landingFeaturedIDs size = %d; want %d (the landing layout assumes exactly three featured picks)", got, want)
	}
}

// TestLandingDefaultTemplateIsFeatured guards that the pre-checked default
// is one of the visible cards. If the default ID drifts out of the featured
// list, the form would render with no card checked and silently POST an
// empty template, falling back to "blank" via templates.Get().
func TestLandingDefaultTemplateIsFeatured(t *testing.T) {
	for _, id := range landingFeaturedIDs {
		if id == landingDefaultTemplateID {
			return
		}
	}
	t.Errorf("landingDefaultTemplateID=%q is not in landingFeaturedIDs=%v; the default radio would render unchecked",
		landingDefaultTemplateID, landingFeaturedIDs)
}
