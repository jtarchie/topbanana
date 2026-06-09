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
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/templates"
	"github.com/jtarchie/topbanana/internal/textedit"
)

// The MCP surface lets an external agent (Claude Code) own the authoring loop:
// it reads and writes a site's files directly instead of paying for the
// server-side ADK build agent. Every tool here is deterministic — S3 reads /
// writes, metadata, slug allocation, and the pure-CPU lint pass — so nothing in
// this file ever calls an LLM. Structure mirrors pocketci's server/mcp_server.go.
const (
	mcpServerName    = "topbanana"
	mcpServerVersion = "1.0.0"

	mcpInstructions = "Tools for editing the static HTML sites a user already owns on Top " +
		"Banana. Sites are created in the web UI — start by calling list_sites to find one, " +
		"then get_site to see its pages. Read with read_file; change pages with edit_file " +
		"(surgical find/replace — prefer it over rewriting a whole page), replace_lines, " +
		"insert_at_line, or write_file (whole file; also for text assets like favicon.svg). " +
		"For binary images (png/jpg/gif/webp), call create_upload_ticket and curl the file to " +
		"the URL it returns — base64 through write_file does not work. " +
		"grep_files searches across a site; delete_file removes a page. Keep every page " +
		"self-contained: inline any JS, no external CDNs, relative links between pages, an " +
		"index.html entry point, and link the self-hosted stylesheet " +
		"`<link rel=\"stylesheet\" href=\"/app.css\">` in <head> (Tailwind utility + daisyUI " +
		"component classes; set the palette with <html data-theme>) — the platform compiles " +
		"/app.css per site. Run lint_site when you finish: it compiles /app.css and reports " +
		"anything to fix. For conventions read the resources topbanana://guide/authoring and " +
		"topbanana://guide/design, and the site's template at topbanana://templates/{id}; the " +
		"edit_page and add_function prompts scaffold common tasks. list_runs / " +
		"get_run_transcript surface read-only build history. All tools are scoped to sites " +
		"the caller owns."
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
	s.registerReadFile(srv)
	s.registerWriteFile(srv)
	s.registerCreateUploadTicket(srv)
	s.registerEditFile(srv)
	s.registerReplaceLines(srv)
	s.registerInsertAtLine(srv)
	s.registerGrepFiles(srv)
	s.registerListFiles(srv)
	s.registerDeleteFile(srv)
	s.registerLintSite(srv)
	s.registerListRuns(srv)
	s.registerGetRunTranscript(srv)

	s.registerWriteFunction(srv)
	s.registerReadFunction(srv)
	s.registerEditFunction(srv)
	s.registerDeleteFunction(srv)
	s.registerListFunctions(srv)
	s.registerTestFunction(srv)
	s.registerConfigureSite(srv)
	s.registerListSubmissions(srv)

	s.registerGuideResources(srv)
	s.registerTemplateResources(srv)
	s.registerFunctionsGuide(srv)
	s.registerEditPrompts(srv)

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
	return s.authorizeSlugOwner(ctx, email, slug)
}

// authorizeSlugOwner looks up email's user record and enforces the ownership
// rule for a slug: super admins reach every slug; everyone else only their
// own; a non-owner sees "not found" so a slug's existence never leaks. slug ==
// "" skips the per-slug check. Shared by the MCP tools (caller from the bearer
// token) and the upload-ticket handler (caller from the signed ticket).
func (s *Server) authorizeSlugOwner(ctx context.Context, email, slug string) (*auth.User, error) {
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
		if user.Role != auth.RoleSuperAdmin && s.registry.ownerOf(slug) != user.Email {
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

// mcpLintNudge is appended to every write/edit result so the external agent
// knows the one ritual that publishes its change: lint_site compiles and
// self-hosts /app.css, without which the page renders unstyled.
const mcpLintNudge = "run lint_site to compile /app.css and publish your changes"

// mcpMaxFileBytes caps a single file written or edited over MCP. Mirrors the
// in-process build agent's per-file limit so neither surface can balloon a
// page past what the proxy/store expect.
const mcpMaxFileBytes = 256 * 1024

// mcpPageURL is the public URL of one page within a site. index.html and the
// empty path resolve to the site root; everything else is appended so the
// agent can open (and, with its own browser tools, see) the exact page it just
// edited.
func (s *Server) mcpPageURL(slug, p string) string {
	base := s.mcpSiteURL(slug)
	if p == "" || p == "index.html" {
		return base
	}
	return base + "/" + p
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

// --- site management --------------------------------------------------------

type listSitesInput struct{}

type siteSummary struct {
	Slug        string    `json:"slug"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	Template    string    `json:"template,omitempty"`
	Created     time.Time `json:"created,omitempty"`
	Private     bool      `json:"private,omitempty"`
	Domains     []string  `json:"domains,omitempty"`
	URL         string    `json:"url"`
}

func (s *Server) registerListSites(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_sites",
		Description: "List the sites the authenticated user owns, with title, template, creation time, privacy flag, any custom domains, and public URL.",
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
			if user.Role != auth.RoleSuperAdmin && s.registry.ownerOf(slug) != user.Email {
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
				Domains:     meta.Domains,
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
		Description: "Get metadata (including any custom domains) and the file list for one site the caller owns.",
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
			"domains":           meta.Domains,
			"url":               s.mcpSiteURL(in.Slug),
			"files":             files,
		})
	})
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
	Path    string `json:"path"    jsonschema:"File path within the site, e.g. index.html, about.html, or an asset like favicon.svg. HTML pages must be self-contained (inline CSS/JS, no external CDNs); image assets (.svg/.png/.jpg/.gif/.webp) are also supported and served with the right content type."`
	Content string `json:"content" jsonschema:"Full file contents to write (overwrites any existing file at this path)."`
}

func (s *Server) registerWriteFile(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "write_file",
		Description: "Create or overwrite a text file in a site the caller owns — HTML pages and text assets like favicon.svg (content type inferred from the extension). For binary images (png/jpg/gif/webp) use create_upload_ticket instead; base64 through write_file does not work.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in writeFileInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		if len(in.Content) > mcpMaxFileBytes {
			return nil, nil, fmt.Errorf("content too large: %d bytes (max %d)", len(in.Content), mcpMaxFileBytes)
		}
		// Snapshot the prior content (if any) so the transcript carries a real
		// before/after diff for overwrites; best-effort, empty for a create.
		before := s.mcpPriorContent(ctx, in.Slug, in.Path)
		err = s.store.Write(ctx, in.Slug, in.Path, in.Content, mcpContentType(in.Path), nil)
		if err != nil {
			return nil, nil, fmt.Errorf("write %q: %w", in.Path, err)
		}
		s.mcpRecordEdit(ctx, in.Slug, "write_file", in.Path, before, in.Content)
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": in.Path,
			"url": s.mcpPageURL(in.Slug, in.Path), "next": mcpLintNudge,
		})
	})
}

// --- surgical editing -------------------------------------------------------
//
// These mirror the in-process build agent's edit tools (internal/agent) and
// share their exact semantics via internal/textedit, so an external agent can
// iterate on a large page without re-sending the whole file. Each gates on
// ValidateHTMLPath (HTML pages only; functions have their own tools), reads
// the current file, applies a pure transform, enforces the per-file cap, and
// writes back — returning the page URL and the lint nudge.

type editFileInput struct {
	Slug       string `json:"slug"                  jsonschema:"The site slug"`
	Path       string `json:"path"                  jsonschema:"HTML page path within the site, e.g. index.html"`
	OldText    string `json:"old_text"              jsonschema:"Exact text to find. Must be unique unless replace_all is true. Whitespace-tolerant fallback applies when no exact match is found."`
	NewText    string `json:"new_text"              jsonschema:"Replacement text."`
	ReplaceAll bool   `json:"replace_all,omitempty" jsonschema:"Replace every occurrence instead of requiring a unique match."`
}

func (s *Server) registerEditFile(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "edit_file",
		Description: "Surgically edit an HTML page in a site the caller owns: old_text must byte-match (and be unique unless replace_all=true). Prefer this over write_file for changes to an existing page — it's cheaper and won't clobber the rest of the file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in editFileInput) (*mcp.CallToolResult, any, error) {
		var count int
		var note string
		_, err := s.mcpApplyToFile(ctx, in.Slug, "edit_file", in.Path, func(content string) (string, error) {
			edit, aerr := textedit.ApplyEdit(content, in.OldText, in.NewText, in.ReplaceAll)
			if aerr != nil {
				return "", aerr
			}
			count, note = edit.Count, edit.Note
			return edit.Content, nil
		})
		if err != nil {
			return nil, nil, err
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": in.Path, "replacements": count, "note": note,
			"url": s.mcpPageURL(in.Slug, in.Path), "next": mcpLintNudge,
		})
	})
}

type replaceLinesInput struct {
	Slug      string `json:"slug"       jsonschema:"The site slug"`
	Path      string `json:"path"       jsonschema:"HTML page path within the site"`
	StartLine int    `json:"start_line" jsonschema:"First line to replace (1-indexed, inclusive)."`
	EndLine   int    `json:"end_line"   jsonschema:"Last line to replace (1-indexed, inclusive). Line numbers must reflect the current file — re-read between edits."`
	NewText   string `json:"new_text"   jsonschema:"Replacement text for the range. Empty deletes the lines."`
}

func (s *Server) registerReplaceLines(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "replace_lines",
		Description: "Replace lines start_line..end_line (1-indexed, inclusive) of an HTML page in a site the caller owns. Empty new_text deletes the range.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in replaceLinesInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpApplyToFile(ctx, in.Slug, "replace_lines", in.Path, func(content string) (string, error) {
			return textedit.SpliceLines(content, in.StartLine, in.EndLine, in.NewText)
		})
		if err != nil {
			return nil, nil, err
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": in.Path,
			"url": s.mcpPageURL(in.Slug, in.Path), "next": mcpLintNudge,
		})
	})
}

type insertAtLineInput struct {
	Slug      string `json:"slug"       jsonschema:"The site slug"`
	Path      string `json:"path"       jsonschema:"HTML page path within the site"`
	AfterLine int    `json:"after_line" jsonschema:"Insert after this line (1-indexed). 0 prepends; total_lines appends."`
	Content   string `json:"content"    jsonschema:"Text to insert verbatim. Include a trailing newline if needed."`
}

func (s *Server) registerInsertAtLine(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "insert_at_line",
		Description: "Insert content after a given line in an HTML page in a site the caller owns, without replacing anything. after_line=0 prepends, after_line=total_lines appends.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in insertAtLineInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpApplyToFile(ctx, in.Slug, "insert_at_line", in.Path, func(content string) (string, error) {
			return textedit.InsertAfterLine(content, in.AfterLine, in.Content)
		})
		if err != nil {
			return nil, nil, err
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": in.Path,
			"url": s.mcpPageURL(in.Slug, in.Path), "next": mcpLintNudge,
		})
	})
}

// mcpPriorContent reads the current content at path, best-effort, to use as
// the before-snapshot of a recorded edit. Returns "" when the file is absent
// or unreadable (a create has no prior content).
func (s *Server) mcpPriorContent(ctx context.Context, slug, path string) string {
	prev, err := s.store.Read(ctx, slug, path)
	if err != nil || prev == nil {
		return ""
	}
	return prev.Content
}

// mcpRecordEdit logs a direct MCP file mutation to build history under the
// "mcp" log key and trims to the configured retention, mirroring the tail of
// build.Service.Start. MCP edit tools write to the store directly, so without
// this they'd leave no transcript — invisible to list_runs, the /system
// dashboard's Recent builds / Last edited, and the Debug viewer. Best-effort
// and synchronous (the request ctx is still live before the tool returns).
func (s *Server) mcpRecordEdit(ctx context.Context, slug, tool, path, before, after string) {
	editrec.RecordEdit(ctx, s.store, slug, "mcp", tool, path, before, after)
	if s.systemInfo.EditsKeep > 0 {
		editrec.Trim(ctx, s.store, slug, s.systemInfo.EditsKeep)
	}
}

// mcpApplyToFile is the shared read-modify-write the surgical edit tools run:
// authorize, validate the HTML path, read the current file (error if missing),
// apply the caller's pure transform, enforce the per-file cap, and write back
// preserving the stored content type. Returns the updated content; edit_file
// captures its replacement count/note through the transform closure. tool names
// the calling MCP tool so the recorded transcript attributes the change.
func (s *Server) mcpApplyToFile(ctx context.Context, slug, tool, p string, transform func(string) (string, error)) (string, error) {
	_, err := s.mcpUserAndAuthorize(ctx, slug)
	if err != nil {
		return "", err
	}
	err = textedit.ValidateHTMLPath(p)
	if err != nil {
		return "", err
	}
	obj, err := s.store.Read(ctx, slug, p)
	if err != nil {
		return "", fmt.Errorf("read %q: %w", p, err)
	}
	if obj.Content == "" {
		return "", fmt.Errorf("file %q not found", p)
	}
	updated, err := transform(obj.Content)
	if err != nil {
		return "", err
	}
	if len(updated) > mcpMaxFileBytes {
		return "", fmt.Errorf("content too large after edit: %d bytes (max %d)", len(updated), mcpMaxFileBytes)
	}
	contentType := obj.ContentType
	if contentType == "" {
		contentType = "text/html; charset=utf-8"
	}
	err = s.store.Write(ctx, slug, p, updated, contentType, obj.Metadata)
	if err != nil {
		return "", fmt.Errorf("write %q: %w", p, err)
	}
	s.mcpRecordEdit(ctx, slug, tool, p, obj.Content, updated)
	return updated, nil
}

type grepFilesInput struct {
	Slug       string `json:"slug"                  jsonschema:"The site slug"`
	Pattern    string `json:"pattern"               jsonschema:"Literal (case-sensitive, no regex) substring to find."`
	MaxResults int    `json:"max_results,omitempty" jsonschema:"Cap on returned matches (default 50, hard cap 200)."`
}

func (s *Server) registerGrepFiles(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "grep_files",
		Description: "Literal substring search across the HTML pages and function handlers of a site the caller owns. Returns paths, 1-indexed line numbers, and snippets — handy for locating a unique string to pass to edit_file.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in grepFilesInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		if in.Pattern == "" {
			return nil, nil, errors.New("pattern is required")
		}
		maxRes := in.MaxResults
		if maxRes <= 0 {
			maxRes = mcpGrepDefaultMax
		}
		if maxRes > mcpGrepHardCap {
			maxRes = mcpGrepHardCap
		}
		files, err := s.store.List(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list files: %w", err)
		}
		sort.Strings(files)
		matches := make([]textedit.GrepMatch, 0, maxRes)
		total, truncated := 0, false
		for _, f := range files {
			if !textedit.GrepEligible(f) {
				continue
			}
			obj, rerr := s.store.Read(ctx, in.Slug, f)
			if rerr != nil || obj.Content == "" {
				continue
			}
			for _, m := range textedit.MatchLines(f, obj.Content, in.Pattern, mcpGrepSnippetMax) {
				total++
				if len(matches) < maxRes {
					matches = append(matches, m)
				} else {
					truncated = true
				}
			}
		}
		return mcpJSON(map[string]any{
			"slug": in.Slug, "matches": matches, "total_matches": total, "truncated": truncated,
		})
	})
}

const (
	mcpGrepDefaultMax = 50
	mcpGrepHardCap    = 200
	mcpGrepSnippetMax = 200
)

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
		before := s.mcpPriorContent(ctx, in.Slug, in.Path)
		err = s.store.Delete(ctx, in.Slug, in.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("delete %q: %w", in.Path, err)
		}
		s.mcpRecordEdit(ctx, in.Slug, "delete_file", in.Path, before, "")
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": in.Path, "next": mcpLintNudge,
		})
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
		problems, msgs := mcpLintProblems(errs)
		return mcpJSON(map[string]any{
			"slug": in.Slug,
			"ok":   len(errs) == 0,
			// problems is the structured form (file/message/kind/autofixable);
			// errors keeps the flat "file: message" strings for older clients.
			"problems": problems,
			"errors":   msgs,
			"url":      s.mcpSiteURL(in.Slug),
		})
	})
}

// mcpLintProblems shapes lint errors into the structured problems the MCP
// lint_site result carries (file/message/kind/autofixable) plus the flat
// "file: message" strings kept for older clients. autofixable mirrors the build
// loop: a kind in lint.AutoFixers is mechanically repaired (by OptimizeCSS,
// which runs before this lint, for the /app.css link and the viewport meta);
// everything else is for the agent to fix.
func mcpLintProblems(errs []lint.Error) (problems []map[string]any, msgs []string) {
	problems = make([]map[string]any, 0, len(errs))
	msgs = make([]string, 0, len(errs))
	for i := range errs {
		e := &errs[i]
		msgs = append(msgs, e.Error())
		_, fixable := lint.AutoFixers[e.Kind]
		problems = append(problems, map[string]any{
			"file":        e.File,
			"message":     e.Message,
			"kind":        string(e.Kind),
			"autofixable": fixable,
		})
	}
	return problems, msgs
}

// --- form submissions (read-only) -------------------------------------------

const mcpSubmissionsMax = 100

// mcpSubmissionRows maps the column-aligned dataRows the manage page renders
// into self-describing {column: value, _key: id} objects, capping at max and
// reporting whether rows were dropped.
func mcpSubmissionRows(cols []string, rows []dataRow, limit int) (out []map[string]any, truncated bool) {
	if len(rows) > limit {
		rows = rows[:limit]
		truncated = true
	}
	out = make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		obj := map[string]any{"_key": r.Key}
		for i, c := range cols {
			if i < len(r.Values) {
				obj[c] = r.Values[i]
			}
		}
		out = append(out, obj)
	}
	return out, truncated
}

type listSubmissionsInput struct {
	Slug string `json:"slug" jsonschema:"The site slug whose form submissions to read"`
}

func (s *Server) registerListSubmissions(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_submissions",
		Description: "Read the form submissions / key-value entries captured by a site the caller owns (the same data behind the manage page and CSV/JSON export). Newest first, capped at 100. Lets an agent confirm a form it built actually captures data.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listSubmissionsInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		cols, rows, err := s.collectSubmissions(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("load submissions: %w", err)
		}
		out, truncated := mcpSubmissionRows(cols, rows, mcpSubmissionsMax)
		return mcpJSON(map[string]any{
			"slug": in.Slug, "columns": cols, "submissions": out,
			"truncated": truncated,
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
