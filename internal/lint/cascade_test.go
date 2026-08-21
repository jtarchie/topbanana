package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// tinytoolsCSS is the relevant slice of the real stylesheet from the incident:
// a fixed size at the top level, and a smaller one inside a breakpoint that
// must NOT be treated as a conflict.
const tinytoolsCSS = `
.tool{display:flex;gap:18px}
.tool .shot{
  flex:none;width:84px;height:84px;border-radius:9px;display:block;
  object-fit:cover;
}
@media(max-width:520px){
  .tool .shot{width:64px;height:64px;border-radius:7px}
}
`

const linkSiteCSS = `<link rel="stylesheet" href="/site.css">`

func parsePage(t *testing.T, name, body string) pageInfo {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(
		"<html><head>" + linkSiteCSS + "</head><body>" + body + "</body></html>"))
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return collectPageInfo(name, doc)
}

func conflicts(t *testing.T, css, body string) []Error {
	t.Helper()
	rules := map[string][]sizingRule{"site.css": collectSizingRules("site.css", css)}
	return checkCascadeConflicts([]pageInfo{parsePage(t, "index.html", body)}, rules)
}

func TestCascadeConflict_CatchesTheIncident(t *testing.T) {
	t.Parallel()

	errs := conflicts(t, tinytoolsCSS,
		`<a class="tool"><img class="shot" src="/a.png" width="360" height="360" alt="x"></a>`)

	// One per property: the agent should clear both in a single pass rather
	// than burning a lint retry to discover the second.
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want one per sized property: %+v", len(errs), errs)
	}
	if !strings.Contains(errs[0].Message, "width") || !strings.Contains(errs[1].Message, "height") {
		t.Errorf("properties should report in a fixed order, got %q then %q", errs[0].Message, errs[1].Message)
	}
	for _, want := range []string{"site.css", "84px", "360"} {
		if !strings.Contains(errs[0].Message, want) {
			t.Errorf("message missing %q: %s", want, errs[0].Message)
		}
	}
	if errs[0].Kind != KindCascadeConflict {
		t.Errorf("Kind = %q, want %q", errs[0].Kind, KindCascadeConflict)
	}
}

// TestCascadeConflict_IsDeterministic guards the property-ordering fix. Reading
// the declaration map directly made the reported property vary between
// identical runs — measured 168 width / 32 height over 200 calls — so the same
// site produced different lint text, different owner-facing copy, and
// different agent instructions on different builds.
func TestCascadeConflict_IsDeterministic(t *testing.T) {
	t.Parallel()

	want := conflicts(t, `.shot{width:84px;height:84px}`,
		`<img class="shot" src="/a.png" width="360" height="360" alt="x">`)
	if len(want) != 2 {
		t.Fatalf("expected two conflicts, got %d", len(want))
	}
	for range 200 {
		got := conflicts(t, `.shot{width:84px;height:84px}`,
			`<img class="shot" src="/a.png" width="360" height="360" alt="x">`)
		if len(got) != len(want) || got[0].Message != want[0].Message || got[1].Message != want[1].Message {
			t.Fatalf("output varies between identical runs:\n%+v\nvs\n%+v", got, want)
		}
	}
}

// Narrowness matters more than coverage: a lint error gates a build, and the
// agent's only way to clear a false one is to damage the page. Every case here
// is a correct site that must stay silent, and each was a live false positive.
func TestCascadeConflict_StaysQuiet(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		css  string
		html string
	}{{
		// The rule applies only below 520px; the attribute governs the
		// desktop rendering the author intended.
		name: "size overridden only inside a breakpoint",
		css:  `@media(max-width:520px){.shot{width:64px}}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		// The canonical responsive pairing: attributes supply the aspect
		// ratio, CSS makes the element fluid.
		name: "fluid rule alongside sizing attributes",
		css:  `img{max-width:100%;height:auto}`,
		html: `<img src="/a.png" width="600" height="400" alt="a">`,
	}, {
		// On canvas the attributes are the bitmap resolution, not a hint —
		// this is the standard HiDPI pattern.
		name: "hidpi canvas",
		css:  `canvas{width:400px;height:300px}`,
		html: `<canvas width="800" height="600"></canvas>`,
	}, {
		// Exactly the inline-SVG sprite both substrate prompts instruct the
		// agent to emit.
		name: "inline svg sized against its viewBox",
		css:  `.icon{width:16px}`,
		html: `<svg class="icon" width="24" height="24" viewBox="0 0 24 24"></svg>`,
	}, {
		name: "iframe intrinsic box",
		css:  `iframe{height:400px}`,
		html: `<iframe title="v" width="560" height="315"></iframe>`,
	}, {
		// .logo is outside any .sidebar, so the rule cannot select it.
		name: "descendant rule whose ancestor does not match",
		css:  `.sidebar .logo{width:40px}`,
		html: `<header><img class="logo" src="/a.png" width="120" alt="a"></header>`,
	}, {
		name: "child combinator with a non-matching parent",
		css:  `.card > .shot{width:84px}`,
		html: `<div class="card"><div><img class="shot" src="/a.png" width="360" alt="a"></div></div>`,
	}, {
		name: "stylesheet merely restates the attribute",
		css:  `.shot{width:360px}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "percentage width",
		css:  `.shot{width:50%}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "min()/clamp() sizing",
		css:  `.shot{width:min(42%,220px)}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "rule targets a different class",
		css:  `.hero{width:84px}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "state-dependent rule",
		css:  `.shot:hover{width:84px}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "element carries no sizing attribute",
		css:  `.shot{width:84px}`,
		html: `<img class="shot" src="/a.png" alt="a">`,
	}, {
		name: "commented-out rule",
		css:  `/* .shot{width:84px} */`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}, {
		name: "keyframes percentages are not selectors",
		css:  `@keyframes grow{from{width:84px}to{width:200px}}`,
		html: `<img class="shot" src="/a.png" width="360" alt="a">`,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if errs := conflicts(t, c.css, c.html); len(errs) > 0 {
				t.Errorf("false positive: %s", errs[0].Message)
			}
		})
	}
}

// TestCascadeConflict_OnlyStylesheetsThePageLinks: a site can carry a sheet
// that only some pages use, and reporting a conflict against CSS that never
// loads on the page sends the agent to edit a rule with no effect there.
func TestCascadeConflict_OnlyStylesheetsThePageLinks(t *testing.T) {
	t.Parallel()

	rules := map[string][]sizingRule{"site.css": collectSizingRules("site.css", `.shot{width:84px}`)}
	unlinked, err := html.Parse(strings.NewReader(
		`<html><head></head><body><img class="shot" src="/a.png" width="360" alt="a"></body></html>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if errs := checkCascadeConflicts([]pageInfo{collectPageInfo("landing.html", unlinked)}, rules); len(errs) > 0 {
		t.Errorf("reported a conflict against a stylesheet the page never links: %s", errs[0].Message)
	}
	if errs := conflicts(t, `.shot{width:84px}`, `<img class="shot" src="/a.png" width="360" alt="a">`); len(errs) == 0 {
		t.Error("a page that does link the stylesheet should still be checked")
	}
}

func TestCollectSizingRules_ParsingEdgeCases(t *testing.T) {
	t.Parallel()

	t.Run("important is the strongest override, not an invisible one", func(t *testing.T) {
		t.Parallel()
		// The flag left attached made the value fail the length test, so the
		// single most binding rule in CSS was the one case never reported.
		if errs := conflicts(t, `.shot{width:84px !important}`,
			`<img class="shot" src="/a.png" width="360" alt="a">`); len(errs) != 1 {
			t.Errorf("got %d errors, want 1", len(errs))
		}
	})

	t.Run("a leading statement at-rule does not swallow the next selector", func(t *testing.T) {
		t.Parallel()
		for _, prologue := range []string{`@charset "utf-8";`, `@import url("other.css");`, `@layer base, ui;`} {
			got := collectSizingRules("site.css", prologue+"\n.shot{width:84px}")
			if len(got) != 1 {
				t.Errorf("%s -> got %d rules, want 1", prologue, len(got))
			}
		}
	})

	t.Run("breakpoint rules are not collected", func(t *testing.T) {
		t.Parallel()
		rules := collectSizingRules("site.css", tinytoolsCSS)
		if len(rules) != 2 {
			t.Fatalf("got %d rules, want 2 (the two unconditional ones): %+v", len(rules), rules)
		}
		for _, r := range rules {
			if r.value != "84px" {
				t.Errorf("collected a conditional rule: %+v", r)
			}
		}
	})

	t.Run("selector list yields one rule per selector", func(t *testing.T) {
		t.Parallel()
		if got := collectSizingRules("site.css", `.a,.b{width:10px}`); len(got) != 2 {
			t.Errorf("got %d rules, want 2", len(got))
		}
	})
}
