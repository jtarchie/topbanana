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
