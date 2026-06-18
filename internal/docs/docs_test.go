package docs_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/assets"
	"github.com/jtarchie/topbanana/internal/docs"
)

func TestDaisyUIDocsVersion_MatchesVersionsJSON(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("sources", "VERSIONS.json"))
	if err != nil {
		t.Fatalf("read VERSIONS.json: %v", err)
	}
	var v struct {
		DaisyUI string `json:"daisyui"`
	}
	err = json.Unmarshal(raw, &v)
	if err != nil {
		t.Fatalf("parse VERSIONS.json: %v", err)
	}
	if v.DaisyUI != docs.DaisyUIDocsVersion {
		t.Errorf("DaisyUIDocsVersion = %q, VERSIONS.json = %q (run `task vendor:docs`)",
			docs.DaisyUIDocsVersion, v.DaisyUI)
	}
}

// TestDaisyUIDocsVersion_MatchesVendoredCSS is the key drift guard: the
// searchable docs must describe the same daisyUI release whose CSS the platform
// compiles, or the agent learns class names that don't exist in /app.css.
func TestDaisyUIDocsVersion_MatchesVendoredCSS(t *testing.T) {
	if docs.DaisyUIDocsVersion != assets.DaisyUIVersion {
		t.Errorf("docs.DaisyUIDocsVersion = %q but assets.DaisyUIVersion = %q — re-run `task vendor:docs` after bumping daisyUI",
			docs.DaisyUIDocsVersion, assets.DaisyUIVersion)
	}
}

func TestSources_ReportsDaisyUI(t *testing.T) {
	found := false
	for _, s := range docs.Sources() {
		if s.ID == "daisyui" {
			found = true
			if s.Version != docs.DaisyUIDocsVersion {
				t.Errorf("daisyui source version = %q, want %q", s.Version, docs.DaisyUIDocsVersion)
			}
		}
	}
	if !found {
		t.Error("daisyui source not reported by Sources()")
	}
}

func TestCorpus_NonEmptyAndSearchable(t *testing.T) {
	res := docs.Search("button", docs.Options{})
	if len(res) == 0 {
		t.Fatal("empty corpus — run `task vendor:docs`")
	}
	if !strings.Contains(strings.ToLower(res[0].Breadcrumb), "button") {
		t.Errorf("top result for 'button' = %q", res[0].Breadcrumb)
	}
}
