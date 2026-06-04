package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"time"

	mcpauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/templates"
)

// The MCP surface lets an external agent (Claude Code) own the authoring loop:
// it reads and writes a site's files directly instead of paying for the
// server-side ADK build agent. Every tool here is deterministic — S3 reads /
// writes, metadata, slug allocation, and the pure-CPU lint pass — so nothing in
// this file ever calls an LLM. Structure mirrors pocketci's server/mcp_server.go.
const (
	mcpServerName    = "topbanana"
	mcpServerVersion = "1.0.0"

	mcpInstructions = "Tools to manage and author static HTML sites hosted on Top Banana, " +
		"on behalf of the authenticated user. Typical flow: call list_sites to see existing " +
		"sites, or create_site to start a new one (you choose the slug). Then author the site " +
		"with write_file — .html files with an index.html entry point and relative links " +
		"between pages. For styling, link the self-hosted stylesheet with " +
		"`<link rel=\"stylesheet\" href=\"/app.css\">` in <head> and use Tailwind utility + " +
		"daisyUI component classes (set the palette with <html data-theme>); the platform " +
		"compiles and serves /app.css per site. Inline any extra JS; no external CDNs. Use " +
		"read_file / list_files to inspect, delete_file to remove a page, and lint_site when " +
		"you finish — it compiles /app.css and reports anything to fix. list_runs and " +
		"get_run_transcript surface read-only transcripts of prior web-UI builds. All tools " +
		"are scoped to sites the caller owns."
)

// newMCPHandler builds the stateless streamable-HTTP handler that serves the
// MCP protocol. The same *mcp.Server instance is reused for every request; tool
// closures capture *Server so they reach the store, build service, and the
// in-memory ownership index. Bearer-token auth is layered on at mount time in
// New (server.go) via mcpauth.RequireBearerToken.
func (s *Server) newMCPHandler() http.Handler {
	srv := s.buildMCPServer()
	return mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{Stateless: true},
	)
}

func (s *Server) buildMCPServer() *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    mcpServerName,
		Version: mcpServerVersion,
	}, &mcp.ServerOptions{
		Instructions: mcpInstructions,
	})

	s.registerListSites(srv)
	s.registerGetSite(srv)
	s.registerCreateSite(srv)
	s.registerReadFile(srv)
	s.registerWriteFile(srv)
	s.registerListFiles(srv)
	s.registerDeleteFile(srv)
	s.registerLintSite(srv)
	s.registerListRuns(srv)
	s.registerGetRunTranscript(srv)

	return srv
}

// --- shared helpers ---------------------------------------------------------

// mcpJSON marshals v into the single text-content result every tool returns.
func mcpJSON(v any) (*mcp.CallToolResult, any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal result: %w", err)
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(data)}},
	}, nil, nil
}

// mcpCaller resolves the authenticated user's email from the bearer token the
// middleware stashed on the request context. Returns an error (surfaced to the
// client) when the context carries no valid token.
func mcpCaller(ctx context.Context) (string, error) {
	info := mcpauth.TokenInfoFromContext(ctx)
	if info == nil || info.UserID == "" {
		return "", errors.New("unauthenticated: no MCP bearer token on request")
	}
	return auth.NormalizeEmail(info.UserID), nil
}

// mcpUserAndAuthorize resolves the caller, looks up their record, and enforces
// the same ownership rule as the web admin surface (authorizeSlug): super
// admins reach every slug; everyone else only their own. A non-owner sees a
// "not found" error so the existence of someone else's slug never leaks.
func (s *Server) mcpUserAndAuthorize(ctx context.Context, slug string) (*auth.User, error) {
	email, err := mcpCaller(ctx)
	if err != nil {
		return nil, err
	}
	user, err := s.auth.Users.LookupCached(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("unknown user %q", email)
	}
	if user.Disabled {
		return nil, errors.New("user is disabled")
	}
	if slug != "" {
		validateErr := validateSlug(slug)
		if validateErr != nil {
			return nil, fmt.Errorf("invalid slug %q: %w", slug, validateErr)
		}
		if user.Role != auth.RoleSuperAdmin && s.ownerOf(slug) != user.Email {
			return nil, fmt.Errorf("site %q not found", slug)
		}
	}
	return user, nil
}

// mcpSiteURL builds the public URL for a slug, mirroring the subdomain (prod)
// vs. path-based (local dev) split used elsewhere in the server.
func (s *Server) mcpSiteURL(slug string) string {
	host := stripPort(s.domain)
	if fallThroughHosts[host] {
		base := "http://" + s.domain
		if s.port != "" && s.port != "80" {
			base += ":" + s.port
		}
		return base + "/s/" + slug
	}
	return "https://" + slug + "." + s.domain
}

// mcpContentType picks a content type for a written file: HTML gets the
// charset-tagged type the proxy expects; everything else falls back to the
// stdlib extension table. store.Write still validates the path itself.
func mcpContentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	}
	if ct := mime.TypeByExtension(strings.ToLower(path.Ext(p))); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// mcpSlugify reduces an arbitrary title to a candidate slug (lowercase
// alphanumerics joined by single hyphens). validateSlug is still the
// authority; create_site rejects a candidate it doesn't like.
func mcpSlugify(in string) string {
	var b strings.Builder
	lastHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(in)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen && b.Len() > 0 {
				b.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// --- site management --------------------------------------------------------

type listSitesInput struct{}

type siteSummary struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Template    string    `json:"template,omitempty"`
	Created     time.Time `json:"created,omitempty"`
	Private     bool      `json:"private,omitempty"`
	URL         string    `json:"url"`
}

func (s *Server) registerListSites(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_sites",
		Description: "List the sites the authenticated user owns, with title, template, creation time, privacy flag, and public URL.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ listSitesInput) (*mcp.CallToolResult, any, error) {
		user, err := s.mcpUserAndAuthorize(ctx, "")
		if err != nil {
			return nil, nil, err
		}
		slugs, err := s.store.ListApps(ctx)
		if err != nil {
			return nil, nil, fmt.Errorf("list sites: %w", err)
		}
		sort.Strings(slugs)
		out := make([]siteSummary, 0, len(slugs))
		for _, slug := range slugs {
			if user.Role != auth.RoleSuperAdmin && s.ownerOf(slug) != user.Email {
				continue
			}
			meta := s.build.ReadMeta(ctx, slug)
			out = append(out, siteSummary{
				Slug:        slug,
				Title:       meta.Title,
				Description: meta.Description,
				Template:    meta.Template,
				Created:     meta.Created,
				Private:     meta.Private,
				URL:         s.mcpSiteURL(slug),
			})
		}
		return mcpJSON(map[string]any{"sites": out})
	})
}

type getSiteInput struct {
	Slug string `json:"slug" jsonschema:"The site slug (subdomain) to inspect"`
}

func (s *Server) registerGetSite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_site",
		Description: "Get metadata and the file list for one site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getSiteInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		meta := s.build.ReadMeta(ctx, in.Slug)
		files, err := s.store.List(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list files: %w", err)
		}
		sort.Strings(files)
		return mcpJSON(map[string]any{
			"slug":              in.Slug,
			"title":             meta.Title,
			"description":       meta.Description,
			"template":          meta.Template,
			"created":           meta.Created,
			"private":           meta.Private,
			"enables_functions": meta.EnablesFunctions,
			"url":               s.mcpSiteURL(in.Slug),
			"files":             files,
		})
	})
}

type createSiteInput struct {
	Slug     string `json:"slug,omitempty"     jsonschema:"Desired subdomain slug (lowercase letters, digits, hyphens). If omitted, derived from title."`
	Title    string `json:"title,omitempty"    jsonschema:"Human-readable site title stored in metadata."`
	Template string `json:"template,omitempty" jsonschema:"Optional template id recorded in metadata (used by lint_site); files are NOT seeded — you author them with write_file."`
}

func (s *Server) registerCreateSite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_site",
		Description: "Create a new empty site owned by the caller and return its slug + URL. Does NOT run any build agent — author the pages yourself with write_file. Enforces the caller's app quota.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createSiteInput) (*mcp.CallToolResult, any, error) {
		user, err := s.mcpUserAndAuthorize(ctx, "")
		if err != nil {
			return nil, nil, err
		}

		slug := strings.TrimSpace(in.Slug)
		if slug == "" {
			slug = mcpSlugify(in.Title)
		}
		if slug == "" {
			return nil, nil, errors.New("provide a slug (or a title to derive one from)")
		}
		validateErr := validateSlug(slug)
		if validateErr != nil {
			return nil, nil, fmt.Errorf("invalid slug %q: %w", slug, validateErr)
		}

		// Reject collisions: an existing file set or a recorded owner both mean
		// the slug is taken. ownerOf covers freshly-created-but-empty sites that
		// have only the metadata sidecar.
		existing, err := s.store.List(ctx, slug)
		if err != nil {
			return nil, nil, fmt.Errorf("check slug: %w", err)
		}
		if len(existing) > 0 || s.ownerOf(slug) != "" {
			return nil, nil, fmt.Errorf("slug %q is already taken", slug)
		}

		quotaErr := s.mcpCheckQuota(ctx, user)
		if quotaErr != nil {
			return nil, nil, quotaErr
		}

		meta := build.SiteMeta{
			Template: strings.TrimSpace(in.Template),
			Created:  time.Now().UTC(),
			Title:    strings.TrimSpace(in.Title),
			OwnerID:  user.Email,
		}
		writeErr := s.build.WriteMeta(ctx, slug, meta)
		if writeErr != nil {
			return nil, nil, fmt.Errorf("write site metadata: %w", writeErr)
		}
		// Register in the in-memory indexes so the slug resolves for TLS /
		// ownership immediately, exactly like the /build handler does.
		s.markSlug(slug)
		s.setOwner(slug, user.Email)

		return mcpJSON(map[string]any{
			"slug": slug,
			"url":  s.mcpSiteURL(slug),
		})
	})
}

// mcpCheckQuota enforces the per-user owned-app cap. Mirrors the platform rule:
// super admins are unlimited; otherwise the cap is the user's MaxApps, falling
// back to the platform default, with 0 meaning unlimited.
func (s *Server) mcpCheckQuota(ctx context.Context, user *auth.User) error {
	if user.Role == auth.RoleSuperAdmin {
		return nil
	}
	limit := user.Quotas.MaxApps
	if limit == 0 {
		limit = s.auth.QuotaDefaults().MaxApps
	}
	if limit == 0 {
		return nil
	}
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("count owned sites: %w", err)
	}
	owned := 0
	for _, slug := range slugs {
		if s.ownerOf(slug) == user.Email {
			owned++
		}
	}
	if owned >= limit {
		return fmt.Errorf("app quota reached (%d of %d)", owned, limit)
	}
	return nil
}

// --- file operations --------------------------------------------------------

type readFileInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
	Path string `json:"path" jsonschema:"File path within the site, e.g. index.html"`
}

func (s *Server) registerReadFile(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "read_file",
		Description: "Read a file from a site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in readFileInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		obj, err := s.store.Read(ctx, in.Slug, in.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("read %q: %w", in.Path, err)
		}
		return mcpJSON(map[string]any{
			"slug":         in.Slug,
			"path":         in.Path,
			"content":      obj.Content,
			"content_type": obj.ContentType,
		})
	})
}

type writeFileInput struct {
	Slug    string `json:"slug"    jsonschema:"The site slug"`
	Path    string `json:"path"    jsonschema:"File path within the site, e.g. index.html or about.html. Create only .html files with inlined CSS/JS."`
	Content string `json:"content" jsonschema:"Full file contents to write (overwrites any existing file at this path)."`
}

func (s *Server) registerWriteFile(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or overwrite a file in a site the caller owns. Use for authoring pages; .html is stored with the correct content type.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeFileInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		err = s.store.Write(ctx, in.Slug, in.Path, in.Content, mcpContentType(in.Path), nil)
		if err != nil {
			return nil, nil, fmt.Errorf("write %q: %w", in.Path, err)
		}
		return mcpJSON(map[string]any{"ok": true, "slug": in.Slug, "path": in.Path})
	})
}

type listFilesInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
}

func (s *Server) registerListFiles(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_files",
		Description: "List all file paths in a site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listFilesInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		files, err := s.store.List(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list files: %w", err)
		}
		sort.Strings(files)
		return mcpJSON(map[string]any{"slug": in.Slug, "files": files})
	})
}

type deleteFileInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
	Path string `json:"path" jsonschema:"File path within the site to delete"`
}

func (s *Server) registerDeleteFile(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "delete_file",
		Description: "Delete a file from a site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in deleteFileInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		err = s.store.Delete(ctx, in.Slug, in.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("delete %q: %w", in.Path, err)
		}
		return mcpJSON(map[string]any{"ok": true, "slug": in.Slug, "path": in.Path})
	})
}

// --- lint -------------------------------------------------------------------

type lintSiteInput struct {
	Slug string `json:"slug" jsonschema:"The site slug to lint"`
}

func (s *Server) registerLintSite(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "lint_site",
		Description: "Compile the site's self-hosted /app.css (Tailwind + daisyUI) and run the deterministic lint checks (no LLM) against a site the caller owns, returning any problems to fix. An empty list means the site passed. Run this when you finish authoring — it's also what publishes the compiled stylesheet.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in lintSiteInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		// Compile + self-host /app.css before linting, mirroring the web build
		// flow (Service.Start). This also injects the /app.css link into pages
		// that lack it, so the design-substrate check below passes. Without it,
		// MCP-authored sites would link a stylesheet nothing ever compiles.
		s.build.OptimizeCSS(ctx, in.Slug)

		meta := s.build.ReadMeta(ctx, in.Slug)
		var tmpl *templates.SiteTemplate
		if meta.Template != "" {
			tmpl = templates.Get(meta.Template) // nil when the id is unknown
		}
		errs := s.build.Lint(ctx, in.Slug, tmpl)
		msgs := make([]string, 0, len(errs))
		for _, e := range errs {
			msgs = append(msgs, e.Error())
		}
		return mcpJSON(map[string]any{
			"slug":   in.Slug,
			"ok":     len(msgs) == 0,
			"errors": msgs,
		})
	})
}

// --- run transcripts (read-only) -------------------------------------------

type listRunsInput struct {
	Slug string `json:"slug" jsonschema:"The site slug whose run transcripts to list"`
}

func (s *Server) registerListRuns(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_runs",
		Description: "List transcript keys for prior builds/edits of a site the caller owns. Pass a key to get_run_transcript to read one.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listRunsInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		keys, err := s.store.ListPrefix(ctx, editrec.Prefix+in.Slug+"/")
		if err != nil {
			return nil, nil, fmt.Errorf("list runs: %w", err)
		}
		sort.Sort(sort.Reverse(sort.StringSlice(keys)))
		return mcpJSON(map[string]any{"slug": in.Slug, "runs": keys})
	})
}

type getRunTranscriptInput struct {
	Slug string `json:"slug" jsonschema:"The site slug the transcript belongs to"`
	Key  string `json:"key"  jsonschema:"The transcript key from list_runs"`
}

func (s *Server) registerGetRunTranscript(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_run_transcript",
		Description: "Read one build/edit transcript (tool calls, file changes, token usage) for a site the caller owns.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getRunTranscriptInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		// Scope the key to this slug's transcript prefix so a caller can't read
		// another site's transcript by passing an arbitrary key.
		want := editrec.Prefix + in.Slug + "/"
		if !strings.HasPrefix(in.Key, want) {
			return nil, nil, fmt.Errorf("transcript key must be under %q", want)
		}
		tr, err := editrec.Read(ctx, s.store, in.Key)
		if err != nil {
			return nil, nil, fmt.Errorf("read transcript: %w", err)
		}
		return mcpJSON(tr)
	})
}
