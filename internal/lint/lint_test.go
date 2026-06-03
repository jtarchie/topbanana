package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestCheckLinkAPIRoutes(t *testing.T) {
	t.Parallel()

	// File set is intentionally empty: /api/* routes don't have backing files,
	// the test only verifies the enablesFns gate.
	fileSet := map[string]bool{
		"index.html": true,
		"about.html": true,
	}

	cases := []struct {
		name       string
		raw        string
		enablesFns bool
		wantErr    bool
	}{
		{"absolute /api/ allowed when functions enabled", "/api/sign", true, false},
		{"absolute /api/ rejected when functions disabled", "/api/sign", false, true},
		{"static link still validated when functions enabled", "missing.html", true, true},
		{"static link to existing file passes either way", "about.html", true, false},
		{"static link to existing file passes either way (off)", "about.html", false, false},
		{"deep /api/ subpath allowed", "/api/cart/add", true, false},
		// Relative "api/foo" is not a dynamic route — it'd resolve to a static
		// file under the page's directory. We do NOT skip those.
		{"relative api/ still validated when functions enabled", "api/sign", true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := checkLink("index.html", ".", tc.raw, fileSet, tc.enablesFns)
			if tc.wantErr && got == nil {
				t.Fatalf("checkLink(%q, enablesFns=%v) = nil, want error", tc.raw, tc.enablesFns)
			}
			if !tc.wantErr && got != nil {
				t.Fatalf("checkLink(%q, enablesFns=%v) = %v, want nil", tc.raw, tc.enablesFns, got)
			}
		})
	}
}

// TestSuspiciousAttrValues_SwallowedLink pins the exact failure pattern
// that shipped a broken DaisyUI page: a viewport <meta> whose content="..."
// is missing a closing quote, so the parser absorbs the following <link>
// into the meta tag's attribute value. golang.org/x/net/html recovers
// silently — html.Parse returns no error — so the bug is only visible via
// the attribute-value contents, never via parse error.
func TestSuspiciousAttrValues_SwallowedLink(t *testing.T) {
	t.Parallel()

	// Note the missing closing quote after "initial-scale".
	src := `<!DOCTYPE html>
<html><head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale        <link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<title>x</title>
</head><body></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}

	errs := suspiciousAttrValues("index.html", doc)
	if len(errs) == 0 {
		t.Fatal("suspiciousAttrValues found no issues; expected to flag the swallowed <link>")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "<link>") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("none of the errors mention the swallowed <link>: %+v", errs)
	}
}

// TestSuspiciousAttrValues_LegitContent confirms we don't false-positive
// on attribute values that legitimately contain "<" or ">" — onclick
// handlers, title text with comparisons, etc.
func TestSuspiciousAttrValues_LegitContent(t *testing.T) {
	t.Parallel()

	cases := []string{
		`<html><body><button onclick="if (x < 5) doThing()">hi</button></body></html>`,
		`<html><body><a title="A > B comparison">link</a></body></html>`,
		`<html><body><input value="x<y, but ok"></body></html>`,
	}

	for _, src := range cases {
		doc, err := html.Parse(strings.NewReader(src))
		if err != nil {
			t.Fatalf("html.Parse(%q): %v", src, err)
		}
		errs := suspiciousAttrValues("index.html", doc)
		if len(errs) != 0 {
			t.Fatalf("suspiciousAttrValues(%q) = %+v, want no errors", src, errs)
		}
	}
}

// TestCheckDesignSubstrate_SwallowedTagsFailLint complements the parser
// fix: when DaisyUI's bytes appear inside a malformed meta's attribute
// value (no real <link> element in the DOM), checkDesignSubstrate must
// still flag the page as missing the substrate.
func TestCheckDesignSubstrate_SwallowedTagsFailLint(t *testing.T) {
	t.Parallel()

	// DaisyUI URL is present as text but the link tag is swallowed by the
	// broken meta. Tailwind script is well-formed (separate line, intact).
	src := `<!DOCTYPE html>
<html><head>
<meta name="viewport" content="width=device-width, initial-scale        <link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
<title>x</title>
</head><body></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}

	errs := checkDesignSubstrate("index.html", doc)
	if len(errs) == 0 {
		t.Fatal("checkDesignSubstrate passed when DaisyUI <link> was swallowed by malformed meta")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "DaisyUI") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a DaisyUI-specific error, got: %+v", errs)
	}
}

// TestCheckDesignSubstrate_WellFormedPasses confirms a correctly authored
// page with both substrate tags as real elements passes lint.
func TestCheckDesignSubstrate_WellFormedPasses(t *testing.T) {
	t.Parallel()

	src := `<!DOCTYPE html>
<html><head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
<title>x</title>
</head><body><h1>hi</h1></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}

	errs := checkDesignSubstrate("index.html", doc)
	if len(errs) != 0 {
		t.Fatalf("checkDesignSubstrate flagged a well-formed page: %+v", errs)
	}
}

// TestCheckDesignSubstrate_LocalStylesheetPasses confirms a page that links
// the self-hosted /app.css (the post-build CSS compile output) is treated as
// having the full substrate — the CDN tags are intentionally absent. Without
// this, a re-lint of an optimized site would try to re-inject CDN tags.
func TestCheckDesignSubstrate_LocalStylesheetPasses(t *testing.T) {
	t.Parallel()

	src := `<!DOCTYPE html>
<html data-theme="synthwave"><head>
<meta charset="UTF-8">
<link rel="stylesheet" href="/app.css">
<title>x</title>
</head><body><h1>hi</h1></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	if errs := checkDesignSubstrate("index.html", doc); len(errs) != 0 {
		t.Fatalf("optimized page (links /app.css) should pass substrate lint, got: %+v", errs)
	}

	// And AutoFix must leave it alone (no CDN tags re-injected).
	if out, changed := AutoFixDesignSubstrate(src); changed {
		t.Errorf("AutoFixDesignSubstrate must not touch a /app.css page:\n%s", out)
	}
}

// TestCheckDesignSubstrate_MissingThemesFailsLint locks in the themes.css
// requirement: a page that only loads the base daisyui@5 stylesheet is
// missing 20+ themes' palettes, so any data-theme beyond light/dark renders
// flat. The lint must call this out so the agent (or Theme Studio) fixes it.
func TestCheckDesignSubstrate_MissingThemesFailsLint(t *testing.T) {
	t.Parallel()

	src := `<!DOCTYPE html>
<html><head>
<meta charset="UTF-8">
<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />
<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>
<title>x</title>
</head><body></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}

	errs := checkDesignSubstrate("index.html", doc)
	found := false
	for _, e := range errs {
		if strings.Contains(e.Message, "themes stylesheet") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a themes-stylesheet error, got: %+v", errs)
	}
}

func TestAutoFixDesignSubstrate(t *testing.T) {
	t.Parallel()

	const wantDaisy = `cdn.jsdelivr.net/npm/daisyui@5"`
	const wantThemes = `daisyui@5/themes.css`
	const wantTailwind = `@tailwindcss/browser@4`

	t.Run("injects all three when head is empty", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><title>x</title></head><body></body></html>`
		out, changed := AutoFixDesignSubstrate(in)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if !strings.Contains(out, wantDaisy) || !strings.Contains(out, wantThemes) || !strings.Contains(out, wantTailwind) {
			t.Errorf("missing substrate tags in output: %s", out)
		}
		if !strings.Contains(out, "</head>") || strings.Index(out, "</head>") < strings.Index(out, wantTailwind) {
			t.Errorf("substrate must be injected before </head>: %s", out)
		}
	})

	t.Run("idempotent when all present", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head>` +
			`<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />` +
			`<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />` +
			`<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>` +
			`</head><body></body></html>`
		_, changed := AutoFixDesignSubstrate(in)
		if changed {
			t.Fatal("expected changed=false when substrate already present")
		}
	})

	t.Run("injects only the missing tag", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head>` +
			`<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />` +
			`<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />` +
			`</head><body></body></html>`
		out, changed := AutoFixDesignSubstrate(in)
		if !changed {
			t.Fatal("expected changed=true (tailwind is missing)")
		}
		if strings.Count(out, "daisyui@5\"") != 1 || strings.Count(out, "daisyui@5/themes.css") != 1 {
			t.Errorf("existing tags must not be duplicated: %s", out)
		}
		if !strings.Contains(out, wantTailwind) {
			t.Errorf("tailwind tag must be injected: %s", out)
		}
	})

	t.Run("returns unchanged when no </head>", func(t *testing.T) {
		in := `<html><body></body></html>`
		out, changed := AutoFixDesignSubstrate(in)
		if changed {
			t.Fatal("expected changed=false when </head> is absent")
		}
		if out != in {
			t.Errorf("content must be returned unchanged when fix is skipped")
		}
	})
}

func TestErrorKindClassification(t *testing.T) {
	t.Parallel()

	// Bare doc → all three substrate errors must have the auto-fix kind.
	doc, err := html.Parse(strings.NewReader(`<!DOCTYPE html><html><head></head><body></body></html>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, e := range checkDesignSubstrate("index.html", doc) {
		if e.Kind != KindDesignSubstrate {
			t.Errorf("expected KindDesignSubstrate, got %q for %q", e.Kind, e.Message)
		}
	}

	// Suspicious-attr fixture from the existing test: unclosed viewport
	// content swallows the next <link>.
	doc, err = html.Parse(strings.NewReader(
		`<!DOCTYPE html><html><head>` +
			`<meta name="viewport" content="<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />` +
			`</head><body></body></html>`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	saw := false
	for _, e := range suspiciousAttrValues("index.html", doc) {
		if e.Kind == KindSuspiciousAttr {
			saw = true
		}
	}
	if !saw {
		t.Errorf("expected at least one KindSuspiciousAttr error from the swallowed-link fixture")
	}
}
