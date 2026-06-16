package assets_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/assets"
)

func TestAppCSSEmbedded(t *testing.T) {
	t.Parallel()

	css := string(assets.AppCSS)
	if len(css) == 0 {
		t.Fatal("AppCSS is empty — run `task css` to compile it")
	}
	// daisyUI markers + a couple of utilities the admin chrome relies on.
	for _, want := range []string{"--color-primary", "data-theme=cyberpunk", "data-theme=lemonade", ".navbar", ".side-panel"} {
		if !strings.Contains(css, want) {
			t.Errorf("AppCSS missing %q", want)
		}
	}
	// No CDN leakage in the compiled output.
	for _, bad := range []string{"cdn.jsdelivr.net", "@tailwindcss/browser"} {
		if strings.Contains(css, bad) {
			t.Errorf("AppCSS unexpectedly references %q", bad)
		}
	}
}

func TestDaisyUIVersionMatchesPackage(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("daisyui", "package.json"))
	if err != nil {
		t.Fatalf("read vendored package.json: %v", err)
	}
	var pkg struct {
		Version string `json:"version"`
	}
	err = json.Unmarshal(raw, &pkg)
	if err != nil {
		t.Fatalf("parse package.json: %v", err)
	}
	if pkg.Version != assets.DaisyUIVersion {
		t.Errorf("DaisyUIVersion = %q, vendored package.json = %q", assets.DaisyUIVersion, pkg.Version)
	}
}

func TestGrapesJSVersionsMatchVendored(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("grapesjs", "VERSIONS.json"))
	if err != nil {
		t.Fatalf("read vendored VERSIONS.json: %v", err)
	}
	var v struct {
		GrapesJS string `json:"grapesjs"`
		Preset   string `json:"grapesjs-preset-webpage"`
	}
	err = json.Unmarshal(raw, &v)
	if err != nil {
		t.Fatalf("parse VERSIONS.json: %v", err)
	}
	if v.GrapesJS != assets.GrapesJSVersion {
		t.Errorf("GrapesJSVersion = %q, vendored = %q", assets.GrapesJSVersion, v.GrapesJS)
	}
	if v.Preset != assets.GrapesJSPresetVersion {
		t.Errorf("GrapesJSPresetVersion = %q, vendored = %q", assets.GrapesJSPresetVersion, v.Preset)
	}
}

func TestGrapesJSEmbedded(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"grapes.min.js", "grapes.min.css", "grapesjs-preset-webpage.min.js"} {
		data, err := assets.GrapesJSFS.ReadFile("grapesjs/" + name)
		if err != nil {
			t.Fatalf("embedded grapesjs/%s: %v", name, err)
		}
		if len(data) == 0 {
			t.Errorf("grapesjs/%s is empty — run `task vendor:grapesjs`", name)
		}
	}
	// The bundle must define the global the editor boots from.
	js, _ := assets.GrapesJSFS.ReadFile("grapesjs/grapes.min.js")
	if !strings.Contains(string(js), "grapesjs") {
		t.Error("grapes.min.js does not look like the GrapesJS bundle")
	}
}

func TestExtractDaisyUI(t *testing.T) {
	t.Parallel()

	dir, err := assets.ExtractDaisyUI(t.TempDir())
	if err != nil {
		t.Fatalf("ExtractDaisyUI: %v", err)
	}
	for _, rel := range []string{"index.js", filepath.Join("components", "button.css"), filepath.Join("theme", "synthwave.css")} {
		_, err := os.Stat(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("extracted daisyui missing %s: %v", rel, err)
		}
	}
}

func TestSiteInputCSS(t *testing.T) {
	t.Parallel()

	got := assets.SiteInputCSS("/cache/daisyui")
	for _, want := range []string{
		`@import "tailwindcss";`,
		`@source not "/cache/daisyui";`,
		`@plugin "/cache/daisyui/index.js"`,
		"themes: all;",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("SiteInputCSS missing %q\n--- got ---\n%s", want, got)
		}
	}
}
