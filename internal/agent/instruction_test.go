package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/templates"
)

// A site is styled either by the platform substrate or by a stylesheet it
// authored for itself. Handing the wrong guidance to either one is not a
// cosmetic problem: on a site with its own CSS, every DaisyUI class the agent
// reaches for is outside the site's design language and loses the cascade to
// the site's own unlayered rules regardless, so the advice is worse than
// silence.
func TestBuildInstruction_SelectsStylingRegime(t *testing.T) {
	t.Parallel()

	tmpl := &templates.SiteTemplate{ID: "contact-form"}
	bctx := func(sheets ...string) BuildContext {
		return BuildContext{
			Now:            time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC),
			Slug:           "tinytools",
			SiteURL:        "https://tinytools.apps.topbanana.dev",
			IsEdit:         true,
			OwnStylesheets: sheets,
		}
	}

	platform := buildInstruction(tmpl, nil, bctx())
	byo := buildInstruction(tmpl, nil, bctx("site.css"))

	t.Run("platform regime keeps the component vocabulary", func(t *testing.T) {
		t.Parallel()
		for _, want := range []string{"DaisyUI", "Tailwind utility vocabulary", "data-theme", "hero-content"} {
			if !strings.Contains(platform, want) {
				t.Errorf("platform instruction missing %q", want)
			}
		}
		if strings.Contains(platform, "has its own stylesheet") {
			t.Error("platform instruction leaked the bring-your-own-CSS block")
		}
	})

	t.Run("byo regime names the stylesheet and drops the component vocabulary", func(t *testing.T) {
		t.Parallel()
		for _, want := range []string{"site.css", "Unlayered rules beat layered ones", "presentation *hints*"} {
			if !strings.Contains(byo, want) {
				t.Errorf("byo instruction missing %q", want)
			}
		}
		for _, unwanted := range []string{"Tailwind utility vocabulary", "DaisyUI components to reach for first", "Visual texture"} {
			if strings.Contains(byo, unwanted) {
				t.Errorf("byo instruction still carries %q — the guidance this regime exists to suppress", unwanted)
			}
		}
	})

	t.Run("both regimes keep the regime-agnostic base", func(t *testing.T) {
		t.Parallel()
		for _, want := range []string{"index.html is required", "Page head requirements", "Asking the user for help", "No external origins"} {
			for name, got := range map[string]string{"platform": platform, "byo": byo} {
				if !strings.Contains(got, want) {
					t.Errorf("%s instruction missing base rule %q", name, want)
				}
			}
		}
	})

	t.Run("multiple stylesheets are all named", func(t *testing.T) {
		t.Parallel()
		got := buildInstruction(tmpl, nil, bctx("site.css", "print.css"))
		if !strings.Contains(got, "site.css, print.css") {
			t.Error("both stylesheets should be named in the byo block")
		}
	})
}

// TestBuildInstruction_SubstrateIsCacheStable guards the prompt-caching
// contract in this file's doc comment: the styling block is per-site constant,
// so it must sit in the stable prefix, and two sites in the same regime must
// share every byte up to the per-build context block.
func TestBuildInstruction_SubstrateIsCacheStable(t *testing.T) {
	t.Parallel()

	tmpl := &templates.SiteTemplate{ID: "contact-form"}
	one := buildInstruction(tmpl, nil, BuildContext{Now: time.Now(), Slug: "alpha", SiteURL: "https://alpha.example"})
	two := buildInstruction(tmpl, nil, BuildContext{Now: time.Now(), Slug: "beta", SiteURL: "https://beta.example"})

	prefix := func(s string) string {
		idx := strings.Index(s, "Build context:")
		if idx < 0 {
			t.Fatal("no build context block to split on")
		}
		return s[:idx]
	}
	if prefix(one) != prefix(two) {
		t.Error("two platform-regime sites do not share an identical cacheable prefix")
	}
	if !strings.HasPrefix(one, systemPrompt+"\n\n"+platformSubstrate) {
		t.Error("the styling block must follow the base prompt directly, ahead of the template addendum")
	}
}
