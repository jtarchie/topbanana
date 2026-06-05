package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func TestCheckLinkAPIRoutes(t *testing.T) {
	t.Parallel()

	// /api/{name} is served by functions/{name}.js. The lint accepts such a link
	// when functions are enabled OR a backing functions/{name}.js exists, so a
	// template-less site (enablesFns=false) with a real function still passes.
	fileSet := map[string]bool{
		"index.html":          true,
		"about.html":          true,
		"functions/submit.js": true,
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
		// The fix: a function-backed route passes even on a template-less site
		// where functions report disabled, because functions/submit.js exists.
		{"/api/ backed by functions file passes when functions disabled", "/api/submit", false, false},
		{"/api/ without backing file still rejected when disabled", "/api/ghost", false, true},
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
<meta name="viewport" content="width=device-width, initial-scale        <link rel="stylesheet" href="/app.css" />
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

// TestCheckDesignSubstrate_SwallowedTagsFailLint complements the parser fix:
// when the /app.css href appears inside a malformed meta's attribute value (no
// real <link> element in the DOM), checkDesignSubstrate must still flag the
// page as missing the substrate.
func TestCheckDesignSubstrate_SwallowedTagsFailLint(t *testing.T) {
	t.Parallel()

	// The href is present as text but the <link> is swallowed by the broken
	// meta whose content="..." never closes its quote.
	src := `<!DOCTYPE html>
<html><head>
<meta name="viewport" content="width=device-width, initial-scale        <link rel="stylesheet" href="/app.css">
<title>x</title>
</head><body></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	if errs := checkDesignSubstrate("index.html", doc); len(errs) == 0 {
		t.Fatal("checkDesignSubstrate passed when the /app.css <link> was swallowed by a malformed meta")
	}
}

// TestCheckDesignSubstrate_WellFormedPasses confirms a page that links the
// self-hosted /app.css as a real element passes lint, and AutoFix leaves it
// untouched.
func TestCheckDesignSubstrate_WellFormedPasses(t *testing.T) {
	t.Parallel()

	src := `<!DOCTYPE html>
<html data-theme="synthwave"><head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<link rel="stylesheet" href="/app.css">
<title>x</title>
</head><body><h1>hi</h1></body></html>`

	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	if errs := checkDesignSubstrate("index.html", doc); len(errs) != 0 {
		t.Fatalf("checkDesignSubstrate flagged a well-formed page: %+v", errs)
	}
	if out, changed := AutoFixDesignSubstrate(src); changed {
		t.Errorf("AutoFixDesignSubstrate must not touch a page already linking /app.css:\n%s", out)
	}
}

func TestAutoFixDesignSubstrate(t *testing.T) {
	t.Parallel()

	const want = `href="/app.css"`

	t.Run("injects the stylesheet when head lacks it", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><title>x</title></head><body></body></html>`
		out, changed := AutoFixDesignSubstrate(in)
		if !changed {
			t.Fatal("expected changed=true")
		}
		if !strings.Contains(out, want) {
			t.Errorf("missing /app.css link in output: %s", out)
		}
		if strings.Index(out, want) > strings.Index(out, "</head>") {
			t.Errorf("link must be injected before </head>: %s", out)
		}
	})

	t.Run("idempotent when already present", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><link rel="stylesheet" href="/app.css"></head><body></body></html>`
		out, changed := AutoFixDesignSubstrate(in)
		if changed {
			t.Fatalf("expected changed=false when /app.css already present:\n%s", out)
		}
		if strings.Count(out, want) != 1 {
			t.Errorf("must not duplicate the link: %s", out)
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

	// Bare doc → the substrate error must carry the auto-fix kind.
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
			`<meta name="viewport" content="<link rel="stylesheet" href="/app.css" />` +
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
