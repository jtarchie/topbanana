package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/templates"
)

// The MCP editing surface carries its conventions through MCP-native Resources
// and Prompts, not just one instructions blob. An external agent (Claude Code)
// arrives cold: it has never seen the authoring contract, the design
// substrate, or the template a site was built from. Resources let it pull that
// context on demand; the edit_page prompt scaffolds the common task. This is
// the "different user, different interface" half of the surface — the tools in
// mcp_server.go do the work, the resources/prompts teach how.

// mcpDesignGuide is the self-hosted-CSS cheatsheet served at
// topbanana://guide/design. Kept short and concrete: the one stylesheet, the
// class vocabulary, and how to switch palettes.
const mcpDesignGuide = `# Design substrate

Every page links **one** stylesheet — the platform compiles and self-hosts it
per site:

` + "```html" + `
<link rel="stylesheet" href="/app.css">
` + "```" + `

There are **no** CDN tags. ` + "`/app.css`" + ` is built from the page's own markup
after you run ` + "`lint_site`" + `, so a freshly written page is unstyled until you
lint it.

## Vocabulary
- **Tailwind utility classes** for layout/spacing/typography: ` + "`flex`, `grid`, `gap-4`, `p-6`, `text-lg`, `font-bold`, `max-w-3xl`, `mx-auto`" + `.
- **daisyUI component classes** for ready-made UI: ` + "`btn`, `btn-primary`, `card`, `navbar`, `hero`, `badge`, `alert`, `menu`, `modal`, `table`" + `.

## Themes
Set the palette on the root element; daisyUI ships every theme, so switching is
just an attribute — no recompile:

` + "```html" + `
<html data-theme="corporate">   <!-- or: dark, emerald, synthwave, retro, ... -->
` + "```" + `

## Rules
- Inline any JS in a ` + "`<script>`" + ` tag — no external scripts or frameworks.
- Relative links between pages (` + "`<a href=\"about.html\">`" + `); ` + "`index.html`" + ` is the entry point.
- Keep each page self-contained.`

// mcpTextResource wraps a single text body in the ReadResourceResult shape the
// SDK expects, echoing back the requested URI.
func mcpTextResource(uri, mime, text string) *mcp.ReadResourceResult {
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{URI: uri, MIMEType: mime, Text: text}},
	}
}

// registerGuideResources exposes the authoring contract and design substrate as
// pull-on-demand markdown. The functions runtime guide is registered alongside
// the function tools (mcp_functions.go) so it only appears when those land.
func (s *Server) registerGuideResources(srv *mcp.Server) {
	guides := []struct {
		uri, name, desc, body string
	}{
		{
			"topbanana://guide/authoring",
			"Authoring guide",
			"The contract every Top Banana page follows: self-contained HTML, inline JS, the /app.css substrate, an index.html entry point, relative links.",
			agent.AuthoringGuide(),
		},
		{
			"topbanana://guide/design",
			"Design substrate guide",
			"How styling works: the single self-hosted /app.css, Tailwind utility + daisyUI component classes, and switching palettes with data-theme.",
			mcpDesignGuide,
		},
	}
	for _, g := range guides {
		body := g.body
		srv.AddResource(
			&mcp.Resource{URI: g.uri, Name: g.name, Description: g.desc, MIMEType: "text/markdown"},
			func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return mcpTextResource(req.Params.URI, "text/markdown", body), nil
			},
		)
	}
}

const mcpTemplateURIPrefix = "topbanana://templates/"

// templateByID returns the registered template with exactly this id, or nil.
// Unlike templates.Get (which falls back to the default for unknown ids), an
// exact match is what a resource lookup wants so an unknown id 404s.
func templateByID(id string) *templates.SiteTemplate {
	for _, t := range templates.All() {
		if t.ID == id {
			return t
		}
	}
	return nil
}

// registerTemplateResources exposes the template catalog (exact URI) and each
// template's detail (URI template). The detail is what an editing agent reads
// to learn the conventions of the template a site was built from — its
// authoring addendum and worked examples.
func (s *Server) registerTemplateResources(srv *mcp.Server) {
	srv.AddResource(
		&mcp.Resource{
			URI:         "topbanana://templates",
			Name:        "Template catalog",
			Description: "The site templates available on Top Banana — id, label, description, whether they enable server-side functions, and setup notes.",
			MIMEType:    "application/json",
		},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			type entry struct {
				ID               string `json:"id"`
				Label            string `json:"label"`
				Description      string `json:"description"`
				EnablesFunctions bool   `json:"enables_functions"`
				SetupNotes       string `json:"setup_notes,omitempty"`
			}
			out := make([]entry, 0, len(templates.All()))
			for _, t := range templates.All() {
				out = append(out, entry{t.ID, t.Label, t.Description, t.EnablesFunctions, t.SetupNotes})
			}
			data, err := json.Marshal(map[string]any{"templates": out})
			if err != nil {
				return nil, fmt.Errorf("marshal template catalog: %w", err)
			}
			return mcpTextResource(req.Params.URI, "application/json", string(data)), nil
		},
	)

	srv.AddResourceTemplate(
		&mcp.ResourceTemplate{
			URITemplate: mcpTemplateURIPrefix + "{id}",
			Name:        "Template detail",
			Description: "One template's authoring addendum, setup notes, and worked example pages. Read this for the conventions of the template a site was built from.",
			MIMEType:    "text/markdown",
		},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			id := strings.TrimPrefix(req.Params.URI, mcpTemplateURIPrefix)
			tmpl := templateByID(id)
			if id == "" || tmpl == nil {
				return nil, fmt.Errorf("unknown template %q", id)
			}
			return mcpTextResource(req.Params.URI, "text/markdown", renderTemplateDetail(tmpl)), nil
		},
	)
}

// renderTemplateDetail formats a template as the markdown an agent reads before
// editing a site built from it.
func renderTemplateDetail(t *templates.SiteTemplate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n", t.Label, t.Description)
	if t.EnablesFunctions {
		b.WriteString("\nServer-side functions are enabled for this template (functions/<name>.js → /api/<name>).\n")
	}
	if strings.TrimSpace(t.PromptAddendum) != "" {
		fmt.Fprintf(&b, "\n## Authoring notes\n\n%s\n", strings.TrimSpace(t.PromptAddendum))
	}
	if strings.TrimSpace(t.SetupNotes) != "" {
		fmt.Fprintf(&b, "\n## Setup notes\n\n%s\n", strings.TrimSpace(t.SetupNotes))
	}
	if len(t.Examples) > 0 {
		b.WriteString("\n## Examples\n")
		for _, name := range sortedKeys(t.Examples) {
			fmt.Fprintf(&b, "\n### %s\n\n```html\n%s\n```\n", name, t.Examples[name])
		}
	}
	return b.String()
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// Stable, deterministic order without importing sort here would be odd;
	// the catalog is small so a simple insertion keeps allocations down.
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	return keys
}

// registerEditPrompts registers the guided entry points for editing. add_function
// is registered with the function tools (mcp_functions.go) so it only appears
// when those land.
func (s *Server) registerEditPrompts(srv *mcp.Server) {
	srv.AddPrompt(
		&mcp.Prompt{
			Name:        "edit_page",
			Title:       "Edit a page",
			Description: "Scaffold an edit to one page of a site you own: loads the current page and the site's conventions, then asks you to make a specific change.",
			Arguments: []*mcp.PromptArgument{
				{Name: "slug", Description: "The site slug to edit", Required: true},
				{Name: "page", Description: "Page path (defaults to index.html)"},
				{Name: "goal", Description: "What to change", Required: true},
			},
		},
		s.editPagePromptHandler,
	)
}

func (s *Server) editPagePromptHandler(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	args := req.Params.Arguments
	slug := strings.TrimSpace(args["slug"])
	if slug == "" {
		return nil, errInvalidPromptArg("slug is required")
	}
	_, err := s.mcpUserAndAuthorize(ctx, slug)
	if err != nil {
		return nil, err
	}
	page := strings.TrimSpace(args["page"])
	if page == "" {
		page = "index.html"
	}
	goal := strings.TrimSpace(args["goal"])

	obj, err := s.store.Read(ctx, slug, page)
	if err != nil {
		return nil, fmt.Errorf("read %q: %w", page, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are editing the page %q of the Top Banana site %q (%s).\n\n", page, slug, s.mcpPageURL(slug, page))
	if goal != "" {
		fmt.Fprintf(&b, "Goal: %s\n\n", goal)
	}
	b.WriteString("Conventions to follow (read the resources for detail):\n")
	b.WriteString("- topbanana://guide/authoring — the authoring contract\n")
	b.WriteString("- topbanana://guide/design — the /app.css design substrate\n\n")
	b.WriteString("Prefer edit_file (surgical find/replace) over rewriting the whole page. ")
	b.WriteString("When done, run lint_site to compile /app.css and publish.\n\n")
	if obj.Content == "" {
		fmt.Fprintf(&b, "The page %q does not exist yet — create it with write_file.\n", page)
	} else {
		fmt.Fprintf(&b, "Current contents of %s:\n\n```html\n%s\n```\n", page, obj.Content)
	}

	return &mcp.GetPromptResult{
		Description: fmt.Sprintf("Edit %s of %s", page, slug),
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: b.String()}},
		},
	}, nil
}

func errInvalidPromptArg(msg string) error {
	return fmt.Errorf("invalid prompt argument: %s", msg)
}
