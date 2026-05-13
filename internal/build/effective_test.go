package build_test

import (
	"testing"

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/templates"
)

// TestEffectiveTemplate walks the four (template-enables × meta-override)
// truth-table cases. Brochure-template + meta-off (default) stays brochure;
// brochure-template + meta-on flips functions ON without mutating the registry
// singleton; function-template ignores meta because templates are already
// the strongest "on" signal.
//
//nolint:cyclop // table-driven test with one extra registry-mutation assertion per row; splitting hurts readability.
func TestEffectiveTemplate(t *testing.T) {
	// Find a function-enabling template that exists in the registry. Ship
	// templates may change over time; we just need any one of them.
	var enablingID, brochureID string
	for _, tmpl := range templates.All() {
		switch {
		case tmpl.EnablesFunctions && enablingID == "":
			enablingID = tmpl.ID
		case !tmpl.EnablesFunctions && brochureID == "":
			brochureID = tmpl.ID
		}
	}
	if enablingID == "" || brochureID == "" {
		t.Fatalf("registry must have both a function-enabling and a brochure template; got enabling=%q brochure=%q", enablingID, brochureID)
	}

	cases := []struct {
		name        string
		templateID  string
		overrideOn  bool
		wantEnabled bool
	}{
		{"brochure + no override", brochureID, false, false},
		{"brochure + override on", brochureID, true, true},
		{"function template + no override", enablingID, false, true},
		{"function template + override on", enablingID, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			meta := build.SiteMeta{Template: tc.templateID, EnablesFunctions: tc.overrideOn}
			got := build.EffectiveTemplate(meta)
			if got == nil {
				t.Fatal("got nil template")
			}
			if got.EnablesFunctions != tc.wantEnabled {
				t.Fatalf("EnablesFunctions=%v, want %v", got.EnablesFunctions, tc.wantEnabled)
			}

			// Critical: never mutate the registry singleton when flipping on.
			registry := templates.Get(tc.templateID)
			if registry == got && tc.overrideOn && !registry.EnablesFunctions {
				t.Fatal("EffectiveTemplate returned the registry singleton after flipping the override on — must be a copy")
			}
			// And the registry's bit must never change.
			if registry.EnablesFunctions != (tc.templateID == enablingID) {
				t.Fatalf("registry singleton was mutated: got EnablesFunctions=%v", registry.EnablesFunctions)
			}
		})
	}

	// Unknown template returns nil.
	t.Run("unknown template ID", func(t *testing.T) {
		// `Get` falls back to the default template for unknown IDs, so this is
		// a sanity check that nil-safety is in the helper rather than a real
		// "missing template" path.
		got := build.EffectiveTemplate(build.SiteMeta{Template: "definitely-not-a-real-template", EnablesFunctions: true})
		if got == nil {
			t.Fatal("expected default template, got nil")
		}
	})
}
