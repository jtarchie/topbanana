package templates_test

import (
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/guide"
	"github.com/jtarchie/topbanana/internal/templates"
)

// TestEveryTemplate_GuideIsWellFormed is the contract test for the owner-facing
// completeness guide: every shipped guide item must be fully populated and
// reference a real detector with the params that detector needs. It lives in an
// external test package because it imports internal/guide (which imports
// internal/templates) — keeping it out of package templates avoids an import
// cycle. A new template author who fat-fingers a detector name or forgets the
// keywords gets a loud failure here rather than a silently broken card.
func TestEveryTemplate_GuideIsWellFormed(t *testing.T) {
	t.Parallel()

	for _, tmpl := range templates.All() {
		seen := make(map[string]bool)
		for i, item := range tmpl.Guide {
			if strings.TrimSpace(item.ID) == "" {
				t.Errorf("%s guide[%d]: empty ID", tmpl.ID, i)
				continue
			}
			if seen[item.ID] {
				t.Errorf("%s: duplicate guide item ID %q", tmpl.ID, item.ID)
			}
			seen[item.ID] = true
			checkGuideItem(t, tmpl.ID, item)
		}
	}
}

// validGuideScopes is the set a GuideItem.Scope may take ("" means any-page).
var validGuideScopes = map[string]bool{
	"":               true,
	guide.ScopeAny:   true,
	guide.ScopeEvery: true,
	guide.ScopeFile:  true,
}

// checkGuideItem asserts one item is fully populated and references a real
// detector with the params that detector requires.
func checkGuideItem(t *testing.T, tmplID string, item templates.GuideItem) {
	t.Helper()
	where := tmplID + " guide[" + item.ID + "]"

	if strings.TrimSpace(item.Label) == "" {
		t.Errorf("%s: empty Label", where)
	}
	if strings.TrimSpace(item.Why) == "" {
		t.Errorf("%s: empty Why (the owner needs a reason)", where)
	}
	if strings.TrimSpace(item.How) == "" {
		t.Errorf("%s: empty How (the owner needs a next step)", where)
	}
	if !guide.KnownDetectors()[item.Detector] {
		t.Errorf("%s: unknown detector %q", where, item.Detector)
	}
	if !validGuideScopes[item.Scope] {
		t.Errorf("%s: invalid scope %q", where, item.Scope)
	}

	switch item.Detector {
	case "min_images", "min_links":
		if item.Params.Min <= 0 {
			t.Errorf("%s: detector %q needs params.min > 0", where, item.Detector)
		}
	case "heading_matches", "section_present":
		if len(item.Params.Keywords) == 0 {
			t.Errorf("%s: detector %q needs params.keywords", where, item.Detector)
		}
	}
}

// TestGuide_AtLeastOneTemplateShipsIt locks in that the completeness guide is
// actually exercised by a shipped template — otherwise a regression in the
// frontmatter plumbing would be silent until someone opened the right manage
// page.
func TestGuide_AtLeastOneTemplateShipsIt(t *testing.T) {
	t.Parallel()

	for _, tmpl := range templates.All() {
		if len(tmpl.Guide) > 0 {
			return
		}
	}
	t.Error("no shipped template declares a completeness guide")
}
