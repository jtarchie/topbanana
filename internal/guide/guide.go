// Package guide implements the owner-facing, fully deterministic
// "is my site complete?" checklist. For each site type (template) the
// template declares a set of essential content pieces (templates.GuideItem);
// this package detects — with inspectable rules over the site's real HTML, no
// AI — which are present, and the manage page renders the ✓/✗ result.
//
// It is the advisory counterpart to internal/lint: lint enforces hard build
// invariants for the agent, guide tells the owner what a credible site of this
// type still needs.
package guide

import "github.com/jtarchie/topbanana/internal/templates"

// Scope controls which pages a guide item's detector inspects and how the
// per-page outcomes combine into one present/absent result.
const (
	// ScopeAny (the default, empty string) — present if the detector matches on
	// any page. "The site has a phone number somewhere."
	ScopeAny = "any-page"
	// ScopeEvery — present only if every HTML page matches. "A click-to-call
	// link in the footer of every page."
	ScopeEvery = "every-page"
	// ScopeFile — inspect only the item's Page (default index.html). "The RSVP
	// form is on the home page."
	ScopeFile = "specific-file"
)

// Result pairs one declared essential with whether it was found, plus the
// editor deep link the manage page renders for a missing item.
type Result struct {
	Item         templates.GuideItem
	Present      bool
	WorkspaceURL string
}

// Report is the aggregate the manage page renders: the per-item results and a
// present/total tally for the "N of M essentials" badge.
type Report struct {
	Results []Result
	Present int
	Total   int
}

// Complete reports whether every declared essential is present. False for an
// empty report so a template with no guide never reads as "complete."
func (r Report) Complete() bool { return r.Total > 0 && r.Present == r.Total }
