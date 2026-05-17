package server

import (
	"strings"
	"testing"
)

func TestSetThemeAttribute_ReplacesExisting(t *testing.T) {
	src := `<!DOCTYPE html><html lang="en" data-theme="light"><head><title>t</title></head><body><p>hi</p></body></html>`

	got, err := setThemeAttribute(src, "synthwave")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if !strings.Contains(got, `data-theme="synthwave"`) {
		t.Errorf("new theme not set; got:\n%s", got)
	}
	if strings.Contains(got, `data-theme="light"`) {
		t.Errorf("old theme not replaced; got:\n%s", got)
	}
	if !strings.Contains(got, `lang="en"`) {
		t.Errorf("sibling attribute lost; got:\n%s", got)
	}
	if !strings.Contains(got, "<title>t</title>") || !strings.Contains(got, "<p>hi</p>") {
		t.Errorf("body/head content lost; got:\n%s", got)
	}
}

func TestSetThemeAttribute_AddsWhenMissing(t *testing.T) {
	src := `<!DOCTYPE html><html lang="en"><head><title>t</title></head><body><p>hi</p></body></html>`

	got, err := setThemeAttribute(src, "cupcake")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if !strings.Contains(got, `data-theme="cupcake"`) {
		t.Errorf("attribute not added; got:\n%s", got)
	}
	if !strings.Contains(got, `lang="en"`) {
		t.Errorf("existing attribute dropped; got:\n%s", got)
	}
}

func TestSetThemeAttribute_PreservesDoctype(t *testing.T) {
	src := `<!DOCTYPE html><html data-theme="light"><head></head><body></body></html>`

	got, err := setThemeAttribute(src, "dark")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if !strings.HasPrefix(got, "<!DOCTYPE html>") {
		t.Errorf("doctype lost; got:\n%s", got)
	}
}

func TestSetThemeAttribute_NoHTMLTagErrors(t *testing.T) {
	// A bare fragment with no <html> wrapper still parses (html.Parse
	// synthesizes one), so feed it a doctype-less snippet that fails the
	// findHTMLNode contract only if we tighten it later. For now we accept
	// that html.Parse will always synthesize an <html>, so the missing-tag
	// case is essentially unreachable from a real stored page — this test
	// just locks in the recovery path returning a non-nil result.
	src := `<p>hi</p>`
	got, err := setThemeAttribute(src, "dark")
	if err != nil {
		t.Fatalf("setThemeAttribute on fragment: %v", err)
	}
	if !strings.Contains(got, `data-theme="dark"`) {
		t.Errorf("synthesized <html> didn't get data-theme; got:\n%s", got)
	}
}

func TestSetThemeAttribute_HandlesUppercaseAttribute(t *testing.T) {
	// html.Parse lowercases attribute names, so DATA-THEME → data-theme
	// before our scan. Locks in that behaviour: no duplicate attribute
	// gets emitted.
	src := `<!DOCTYPE html><html DATA-THEME="light"><head></head><body></body></html>`

	got, err := setThemeAttribute(src, "night")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if strings.Count(got, "data-theme") != 1 {
		t.Errorf("expected exactly one data-theme attribute; got:\n%s", got)
	}
	if !strings.Contains(got, `data-theme="night"`) {
		t.Errorf("new theme not set; got:\n%s", got)
	}
}

func TestSetThemeAttribute_AddsMissingThemesCSS(t *testing.T) {
	// A page from before themes.css was required: base daisy + tailwind
	// only. Applying a theme should also bolt on the themes companion.
	src := `<!DOCTYPE html><html data-theme="light"><head>` +
		`<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />` +
		`<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>` +
		`</head><body></body></html>`

	got, err := setThemeAttribute(src, "synthwave")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if !strings.Contains(got, `data-theme="synthwave"`) {
		t.Errorf("new theme not set; got:\n%s", got)
	}
	if !strings.Contains(got, "daisyui@5/themes.css") {
		t.Errorf("themes.css link not injected; got:\n%s", got)
	}
	// The themes link must follow (not precede) the base link so the
	// cascade order matches the agent prompt.
	baseIdx := strings.Index(got, `npm/daisyui@5"`)
	themesIdx := strings.Index(got, "daisyui@5/themes.css")
	if baseIdx < 0 || themesIdx < 0 || themesIdx < baseIdx {
		t.Errorf("themes link must come after base link; got:\n%s", got)
	}
}

func TestSetThemeAttribute_DoesNotDuplicateThemesCSS(t *testing.T) {
	src := `<!DOCTYPE html><html data-theme="light"><head>` +
		`<link href="https://cdn.jsdelivr.net/npm/daisyui@5" rel="stylesheet" type="text/css" />` +
		`<link href="https://cdn.jsdelivr.net/npm/daisyui@5/themes.css" rel="stylesheet" type="text/css" />` +
		`<script src="https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4"></script>` +
		`</head><body></body></html>`

	got, err := setThemeAttribute(src, "cupcake")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if c := strings.Count(got, "daisyui@5/themes.css"); c != 1 {
		t.Errorf("expected exactly one themes link, got %d; output:\n%s", c, got)
	}
}

func TestSetThemeAttribute_LeavesDaisylessPagesAlone(t *testing.T) {
	// If the base daisyui link is missing the page isn't built on the
	// design substrate; Theme Studio swaps the attribute but doesn't try
	// to bootstrap the missing scaffolding.
	src := `<!DOCTYPE html><html data-theme="light"><head><title>t</title></head><body></body></html>`

	got, err := setThemeAttribute(src, "dark")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if strings.Contains(got, "daisyui@5/themes.css") {
		t.Errorf("themes link bootstrapped without a base daisy link; got:\n%s", got)
	}
	if !strings.Contains(got, `data-theme="dark"`) {
		t.Errorf("theme attribute swap still failed; got:\n%s", got)
	}
}

func TestReadThemeAttribute(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
	}{
		{"present", `<html data-theme="synthwave"><body></body></html>`, "synthwave"},
		{"missing", `<html><body></body></html>`, ""},
		{"empty value", `<html data-theme=""><body></body></html>`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readThemeAttribute(tc.src)
			if err != nil {
				t.Fatalf("readThemeAttribute: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDaisyThemeSet_AllowlistMatchesGallery(t *testing.T) {
	if len(daisyThemeSet) != len(daisyThemes) {
		t.Errorf("allowlist size (%d) != gallery size (%d) — every gallery theme must be in the allowlist", len(daisyThemeSet), len(daisyThemes))
	}
	for _, theme := range daisyThemes {
		if !daisyThemeSet[theme.Name] {
			t.Errorf("theme %q in gallery but missing from allowlist", theme.Name)
		}
	}
}

func TestDaisyThemeSet_RejectsUnknown(t *testing.T) {
	// Spot-check a few names that look plausible but aren't real DaisyUI
	// themes (or are themes we deliberately don't ship).
	rejects := []string{"", "tritanopia", "dim", "fantasy", "luxury", "halloween", "../etc/passwd", "<script>"}
	for _, name := range rejects {
		if daisyThemeSet[name] {
			t.Errorf("allowlist accepted unexpected theme %q", name)
		}
	}
}
