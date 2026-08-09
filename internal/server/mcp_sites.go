package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/guide"
	"github.com/jtarchie/topbanana/internal/quotas"
	"github.com/jtarchie/topbanana/internal/templates"
)

// Site creation and the owner-facing completeness checklist. Both are
// deterministic — create_site is slug allocation plus a skeleton write, and
// guide.Evaluate is pure HTML inspection — so neither breaks the rule that no
// tool in the MCP surface calls an LLM.

// mcpPlainErr flattens an echo.HTTPError into an ordinary error. resolveSlug
// and friends are shared with the web handlers and return HTTP-shaped errors;
// surfaced verbatim to an agent those read as "code=409, message=slug is
// already taken", which looks like a transport bug rather than the actionable
// sentence it is.
func mcpPlainErr(err error) error {
	var he *echo.HTTPError
	if errors.As(err, &he) && he.Message != "" {
		return errors.New(he.Message)
	}
	return err
}

// mcpTemplate resolves a template id for create_site. Unlike templates.Get —
// which falls back to "blank" so a stale form value never breaks a build — an
// unknown id is an error here: an agent that mistypes should be corrected, not
// handed a blank site it will then try to treat as a restaurant template.
func mcpTemplate(id string) (*templates.SiteTemplate, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return templates.Get(""), nil
	}
	all := templates.All()
	for _, t := range all {
		if t.ID == id {
			return t, nil
		}
	}
	ids := make([]string, 0, len(all))
	for _, t := range all {
		ids = append(ids, t.ID)
	}
	sort.Strings(ids)
	return nil, fmt.Errorf("unknown template %q (available: %s)", id, strings.Join(ids, ", "))
}

type createSiteInput struct {
	Slug        string `json:"slug,omitempty"        jsonschema:"Requested subdomain slug. Omit to have one generated. Must be unused."`
	Template    string `json:"template,omitempty"    jsonschema:"Template id to seed from (see the topbanana://templates/{id} resources). Defaults to blank."`
	Title       string `json:"title,omitempty"       jsonschema:"Human-readable site title."`
	Description string `json:"description,omitempty" jsonschema:"Short site description."`
}

// registerCreateSite is the one piece of site lifecycle the MCP surface owns.
// Everything creation needs beyond the LLM is already deterministic: allocate
// a slug, write the template skeleton, record the owner. Without it an agent
// could edit sites but never start one, which forced a web-UI detour into the
// front of every authoring session. Deletion, transfer, and remix stay in the
// web UI — they are destructive or rare, and the UI is right there.
func (s *Server) registerCreateSite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_site",
		Description: "Create a new empty site owned by the caller, seeded from a template's skeleton files. No LLM runs — you author the pages yourself with write_file/edit_file afterwards, then lint_site. Returns the new slug, its public URL, the seeded files, and the template's completeness checklist so you know what the site still needs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createSiteInput) (*mcp.CallToolResult, any, error) {
		user, err := s.mcpUserAndAuthorize(ctx, "")
		if err != nil {
			return nil, nil, err
		}
		tmpl, err := mcpTemplate(in.Template)
		if err != nil {
			return nil, nil, err
		}
		// Same cap the web build path enforces; super admins bypass inside
		// CheckMaxApps. Without this the MCP surface would be a way around a
		// quota the UI honours.
		err = quotas.CheckMaxApps(user, s.registry.countAppsFor(user.Email), s.quotaDefaults)
		if err != nil {
			return nil, nil, fmt.Errorf("cannot create site: %w", err)
		}
		slug, err := s.resolveSlug(ctx, strings.TrimSpace(in.Slug))
		if err != nil {
			return nil, nil, mcpPlainErr(err)
		}

		err = s.build.SeedTemplate(ctx, slug, user.Email, tmpl)
		if err != nil {
			return nil, nil, fmt.Errorf("seed template: %w", err)
		}
		// Title/description live on the sidecar SeedTemplate just wrote, so
		// they're a second pass rather than an argument to it.
		title := strings.TrimSpace(in.Title)
		description := strings.TrimSpace(in.Description)
		if title != "" || description != "" {
			meta := s.build.ReadMeta(ctx, slug)
			meta.Title = title
			meta.Description = description
			err = s.build.WriteMeta(ctx, slug, meta)
			if err != nil {
				return nil, nil, fmt.Errorf("write site metadata: %w", err)
			}
		}

		// Register slug + owner before returning so the very next tool call
		// (which will authorize against the registry) sees the site, without
		// waiting on a ListApps sweep — same as buildHandler. Deliberately no
		// full rebuildIndexes: these two entries are everything a brand-new
		// site has (no custom domains, not private), and a rebuild replaces
		// the maps from a fresh sweep, which would drop the markSlug of any
		// web build still in flight — its slug folder doesn't exist in S3 yet,
		// so ListApps can't see it, and HostAllowed would then refuse the TLS
		// handshake for that user's progress page.
		s.registry.markSlug(slug)
		s.registry.setOwner(slug, user.Email)
		slog.Info("site.create", "slug", slug, "template", tmpl.ID, "owner", user.Email, "via", "mcp")

		files, err := s.store.List(ctx, slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list files: %w", err)
		}
		sort.Strings(files)

		res := map[string]any{
			"ok": true, "slug": slug, "url": s.mcpSiteURL(slug),
			"template": tmpl.ID, "title": title, "description": description,
			"enables_functions": tmpl.EnablesFunctions,
			"files":             files,
			"guide":             mcpGuideItems(tmpl),
			"next":              "author the pages with write_file/edit_file, then run " + mcpLintNudge,
		}
		if tmpl.SetupNotes != "" {
			res["setup_notes"] = tmpl.SetupNotes
		}
		return mcpJSON(res)
	})
}

// mcpGuideItems is the template's checklist with no site to evaluate against —
// what create_site returns so the agent starts knowing the target.
func mcpGuideItems(tmpl *templates.SiteTemplate) []map[string]any {
	if tmpl == nil {
		return nil
	}
	out := make([]map[string]any, 0, len(tmpl.Guide))
	for _, item := range tmpl.Guide {
		out = append(out, map[string]any{
			"id": item.ID, "label": item.Label, "why": item.Why, "how": item.How,
			"required": item.Required == nil || *item.Required,
		})
	}
	return out
}

type getSiteGuideInput struct {
	Slug string `json:"slug" jsonschema:"The site slug to evaluate"`
}

// registerGetSiteGuide exposes the completeness checklist the manage page
// renders. It answers a question lint cannot: lint reports what is *broken*,
// the guide reports what a credible site of this type is still *missing* —
// hours on a restaurant page, an RSVP form on an event page. Fully
// deterministic (internal/guide inspects semantic elements in the real HTML),
// so it costs a few reads and no tokens.
func (s *Server) registerGetSiteGuide(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_site_guide",
		Description: "Check which essential content pieces a site the caller owns still lacks, for its site type — opening hours, a contact form, a phone link, and so on. Deterministic (no LLM): each item reports present/absent with why it matters and how to add it. This is the completeness counterpart to lint_site, which only reports what is broken. Templates without a checklist return an empty list.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getSiteGuideInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		meta := s.build.ReadMeta(ctx, in.Slug)
		// Evaluate against the template intrinsic to the site type, matching
		// the manage page: templates.Get falls back to blank (no guide) for an
		// unknown or absent id, which reads as "no checklist" — correct.
		report := guide.Evaluate(ctx, s.store, in.Slug, templates.Get(meta.Template))

		items := make([]map[string]any, 0, len(report.Results))
		missing := make([]string, 0)
		for _, r := range report.Results {
			required := r.Item.Required == nil || *r.Item.Required
			items = append(items, map[string]any{
				"id": r.Item.ID, "label": r.Item.Label, "why": r.Item.Why,
				"how": r.Item.How, "required": required, "present": r.Present,
			})
			if !r.Present && required {
				missing = append(missing, r.Item.Label)
			}
		}
		res := map[string]any{
			"slug": in.Slug, "template": meta.Template,
			"items": items, "present": report.Present, "total": report.Total,
			"complete": report.Complete(),
		}
		if len(missing) > 0 {
			res["next"] = "still missing: " + strings.Join(missing, ", ")
		}
		return mcpJSON(res)
	})
}
