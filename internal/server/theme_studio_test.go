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

func TestSetThemeAttribute_SwapsOnSubstrateLessPage(t *testing.T) {
	// Every theme's palette ships in /app.css, so Theme Studio only swaps the
	// data-theme attribute — it never bootstraps any stylesheet link.
	src := `<!DOCTYPE html><html data-theme="light"><head><link rel="stylesheet" href="/app.css"><title>t</title></head><body></body></html>`

	got, err := setThemeAttribute(src, "dark")
	if err != nil {
		t.Fatalf("setThemeAttribute: %v", err)
	}
	if !strings.Contains(got, `data-theme="dark"`) {
		t.Errorf("theme attribute swap failed; got:\n%s", got)
	}
	if strings.Contains(got, "cdn.jsdelivr.net") {
		t.Errorf("setThemeAttribute must not introduce any CDN link; got:\n%s", got)
	}
	if c := strings.Count(got, `href="/app.css"`); c != 1 {
		t.Errorf("expected exactly one /app.css link, got %d; output:\n%s", c, got)
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
