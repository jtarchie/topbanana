package server

import (
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/templates"
)

// TestMCPPageURL pins the URL the edit tools hand back so the agent can open
// (and, with its own browser, see) the exact page it just changed: index.html
// resolves to the site root, other pages append, and dev (loopback) vs prod
// hosting both work.
func TestMCPPageURL(t *testing.T) {
	t.Parallel()

	dev := &Server{domain: "localhost", port: "8080"}
	cases := map[string]string{
		"index.html": "http://localhost:8080/s/site",
		"about.html": "http://localhost:8080/s/site/about.html",
		"":           "http://localhost:8080/s/site",
	}
	for page, want := range cases {
		if got := dev.mcpPageURL("site", page); got != want {
			t.Errorf("dev mcpPageURL(site, %q) = %q, want %q", page, got, want)
		}
	}

	prod := &Server{domain: "apps.topbanana.dev"}
	if got := prod.mcpPageURL("site", "index.html"); got != "https://site.apps.topbanana.dev" {
		t.Errorf("prod index URL = %q", got)
	}
	if got := prod.mcpPageURL("site", "blog/post.html"); got != "https://site.apps.topbanana.dev/blog/post.html" {
		t.Errorf("prod page URL = %q", got)
	}
}

// TestMCPInstructionsEditFocused pins the surface's rebrand: the instructions
// describe an editing tool (edit_file) and no longer point at create_site,
// which moved to the web UI.
func TestMCPInstructionsEditFocused(t *testing.T) {
	t.Parallel()
	if !strings.Contains(mcpInstructions, "edit_file") {
		t.Error("instructions should mention edit_file")
	}
	if strings.Contains(mcpInstructions, "create_site") {
		t.Error("instructions must not mention the retired create_site")
	}
	if !strings.Contains(mcpLintNudge, "lint_site") {
		t.Errorf("lint nudge should name lint_site, got %q", mcpLintNudge)
	}
}

// TestMCPDesignGuide keeps the served design cheatsheet honest: it must point at
// the self-hosted /app.css and the data-theme palette switch, and must not
// reintroduce a CDN tag.
func TestMCPDesignGuide(t *testing.T) {
	t.Parallel()
	for _, want := range []string{"/app.css", "data-theme", "daisyUI"} {
		if !strings.Contains(mcpDesignGuide, want) {
			t.Errorf("design guide missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(mcpDesignGuide), "jsdelivr") || strings.Contains(mcpDesignGuide, "https://cdn") {
		t.Error("design guide must not point at a CDN host")
	}
}

// TestEmbeddedDesignGuideNonEmpty guards against design_guide.md being
// emptied or accidentally truncated. //go:embed errors at compile time if a
// file is missing, but a zero-byte file would slip through and serve an
// empty MCP resource at topbanana://guide/design.
func TestEmbeddedDesignGuideNonEmpty(t *testing.T) {
	t.Parallel()
	if mcpDesignGuide == "" {
		t.Error("mcpDesignGuide embedded body is empty — was design_guide.md emptied?")
	}
}

func TestTemplateByID(t *testing.T) {
	t.Parallel()
	// The registry always carries the default "blank" template.
	if templateByID("blank") == nil {
		t.Error("templateByID(blank) should resolve")
	}
	if templateByID("definitely-not-a-template") != nil {
		t.Error("unknown id must return nil (not the default fallback)")
	}
	if templateByID("") != nil {
		t.Error("empty id must return nil")
	}
}

func TestRenderTemplateDetail(t *testing.T) {
	t.Parallel()
	tmpl := &templates.SiteTemplate{
		ID:               "demo",
		Label:            "Demo Template",
		Description:      "A demo.",
		PromptAddendum:   "Keep it punchy.",
		SetupNotes:       "Set your API key.",
		EnablesFunctions: true,
		Examples:         map[string]string{"index.html": "<h1>Hi</h1>"},
	}
	out := renderTemplateDetail(tmpl)
	for _, want := range []string{
		"# Demo Template", "A demo.", "Keep it punchy.", "Set your API key.",
		"functions/<name>.js", "index.html", "<h1>Hi</h1>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered detail missing %q\n---\n%s", want, out)
		}
	}
}

func TestSortedKeys(t *testing.T) {
	t.Parallel()
	got := sortedKeys(map[string]string{"c": "", "a": "", "b": ""})
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sortedKeys = %v, want %v", got, want)
	}
	if len(sortedKeys(map[string]string{})) != 0 {
		t.Error("empty map should yield empty slice")
	}
}

func TestMCPTextResource(t *testing.T) {
	t.Parallel()
	res := mcpTextResource("topbanana://guide/design", "text/markdown", "body")
	if len(res.Contents) != 1 {
		t.Fatalf("want 1 content, got %d", len(res.Contents))
	}
	c := res.Contents[0]
	if c.URI != "topbanana://guide/design" || c.MIMEType != "text/markdown" || c.Text != "body" {
		t.Errorf("unexpected content: %+v", c)
	}
}
