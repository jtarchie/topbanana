// Package server is the HTTP layer — Echo routes, handlers, template
// rendering, the subdomain proxy, request validation, and upload handling.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	slogecho "github.com/samber/slog-echo"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/buildabear/internal/agent"
	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/sandbox"
	"github.com/jtarchie/buildabear/internal/snapshot"
	"github.com/jtarchie/buildabear/internal/state"
	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
)

const (
	progressPollIntervalMS = 2000
	progressMaxChecks      = 180
)

// Deps holds the dependencies the server needs. Wired up in cmd/buildabear.
type Deps struct {
	Store             *store.Store
	Build             *build.Service
	Events            *events.Tracker
	LLM               adkmodel.LLM
	Sandbox           *sandbox.Manager
	State             state.Store
	Snapshot          *snapshot.Service
	Domain            string
	Port              string
	AdminUsername     string
	AdminPasswordHash string
}

// Server is the wired-up state shared across handlers.
type Server struct {
	store             *store.Store
	build             *build.Service
	events            *events.Tracker
	llm               adkmodel.LLM
	sandbox           *sandbox.Manager
	state             state.Store
	snapshot          *snapshot.Service
	domain            string
	port              string
	tpl               *template.Template
	adminUsername     string
	adminPasswordHash string

	// domainIndex maps lowercased custom hostnames to the slug that owns
	// them. Rebuilt at startup and after any settings save that touches
	// Domains. Lookups dominate writes, so a single RWMutex is enough.
	domainMu    sync.RWMutex
	domainIndex map[string]string
}

// fallThroughHosts are hosts that should bypass subdomain proxying and hit
// the main routes.
var fallThroughHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"0.0.0.0":   true,
}

// New constructs the Echo server with all routes mounted.
func New(d Deps) *echo.Echo {
	tpl := template.New("")
	// layout.html defines shared partials (e.g. "head") used by the platform
	// pages below. It must be parsed first so the others can reference its
	// blocks.
	template.Must(tpl.Parse(layoutTemplate))
	for _, t := range []struct{ name, body string }{
		{"landing", landingTemplate},
		{"apps", appsTemplate},
		{"progress", progressTemplate},
		{"edit", editTemplate},
		{"settings", settingsTemplate},
		{"toolbar", editToolbarTemplate},
		{"visual_edit", visualEditTemplate},
		{"function_edit", functionEditTemplate},
		{"history", historyTemplate},
		{"data", dataTemplate},
		{"files", filesTemplate},
	} {
		template.Must(tpl.New(t.name).Parse(t.body))
	}

	s := &Server{
		store:             d.Store,
		build:             d.Build,
		events:            d.Events,
		llm:               d.LLM,
		sandbox:           d.Sandbox,
		state:             d.State,
		snapshot:          d.Snapshot,
		domain:            d.Domain,
		port:              d.Port,
		tpl:               tpl,
		adminUsername:     d.AdminUsername,
		adminPasswordHash: d.AdminPasswordHash,
		domainIndex:       map[string]string{},
	}
	s.rebuildDomainIndex(context.Background())

	e := echo.New()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	e.Use(slogecho.New(logger))
	e.Use(s.subdomainMiddleware())

	// /status and /events are reachable unauthed: they're polled by the
	// progress page (which is itself admin-gated) but don't carry any
	// sensitive data, and admins land here mid-build before the cookie has
	// propagated cross-page.
	e.GET("/status/:slug", s.statusHandler)
	e.GET("/events/:slug", s.eventsHandler)

	admin := e.Group("", s.requireAdmin)
	// promptBodyCap bounds the whole request body on prompt-bearing POSTs so a
	// runaway hidden field or selection can't sneak past the per-field caps in
	// the handlers. Leaves /upload/:slug alone — image uploads need 5 MiB.
	promptBodyCap := middleware.BodyLimit(maxPromptBodyBytes)
	admin.GET("/", s.landingHandler)
	admin.POST("/build", s.buildHandler, promptBodyCap)
	admin.GET("/apps", s.appsHandler)
	admin.GET("/edit/:slug", s.editHandler)
	admin.POST("/edit/:slug", s.editSubmitHandler, promptBodyCap)
	admin.POST("/relint/:slug", s.relintHandler)
	admin.GET("/edit/:slug/visual", s.visualEditHandler)
	admin.POST("/edit/:slug/visual", s.visualEditSaveHandler, promptBodyCap)
	admin.GET("/edit/:slug/function/:name", s.functionEditHandler)
	admin.POST("/test/:slug/api/:name", s.functionTestHandler)
	admin.POST("/upload/:slug", s.uploadHandler)
	admin.GET("/settings/:slug", s.settingsHandler)
	admin.POST("/settings/:slug", s.settingsSubmitHandler)
	admin.GET("/history/:slug", s.historyHandler)
	admin.POST("/history/:slug/restore", s.historyRestoreHandler)
	admin.POST("/history/:slug/delete", s.historyDeleteHandler)
	admin.GET("/data/:slug", s.dataHandler)
	admin.GET("/files/:slug", s.filesHandler)

	return e
}

// subdomainMiddleware dispatches by Host:
//
//  1. main domain (or loopback) → admin routes (gated by requireAdmin).
//  2. `*.<domain>` subdomain    → proxy/api for that slug.
//  3. registered custom domain  → proxy/api for the owning slug, with the
//     custom-domain flag set so cache headers go public and the toolbar
//     stays hidden.
//  4. anything else             → 404 (don't let unknown Host headers fall
//     through to admin routes — that's the leak we're closing).
//
// Path-based dispatch inside cases 2 and 3:
//  1. /api/{name}  → apiHandler (only when the template enabled functions)
//  2. anything else → proxyHandler (static)
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			host := stripPort(c.Request().Host)

			if host == s.domain || fallThroughHosts[host] {
				return next(c)
			}

			if slug, ok := strings.CutSuffix(host, "."+s.domain); ok {
				return s.dispatchSite(c, slug)
			}

			if slug, ok := s.lookupCustomDomain(host); ok {
				c.Set("custom_domain", true)
				return s.dispatchSite(c, slug)
			}

			return notFound()
		}
	}
}

// dispatchSite routes a request that's already been mapped to a slug to either
// /api or the static proxy.
func (s *Server) dispatchSite(c *echo.Context, slug string) error {
	reqPath := c.Request().URL.Path
	if name, ok := strings.CutPrefix(reqPath, "/api/"); ok {
		return s.apiHandler(c, slug, name)
	}
	return s.proxyHandler(c, slug)
}

// rebuildDomainIndex scans all sites and rebuilds the host → slug map. Called
// at startup and after any settings save that changes Domains. Errors are
// logged but don't block startup — a partial index just means the affected
// custom domains 404 until the next rebuild.
func (s *Server) rebuildDomainIndex(ctx context.Context) {
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		slog.Warn("domain_index.list_apps_failed", "err", err)
		return
	}
	idx := make(map[string]string, len(apps))
	for _, slug := range apps {
		meta := s.build.ReadMeta(ctx, slug)
		for _, d := range meta.Domains {
			if existing, dup := idx[d]; dup && existing != slug {
				slog.Warn("domain_index.duplicate", "domain", d, "kept", existing, "dropped", slug)
				continue
			}
			idx[d] = slug
		}
	}
	s.domainMu.Lock()
	s.domainIndex = idx
	s.domainMu.Unlock()
	slog.Info("domain_index.rebuilt", "count", len(idx))
}

// lookupCustomDomain returns the slug that owns host, if any.
func (s *Server) lookupCustomDomain(host string) (string, bool) {
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	slug, ok := s.domainIndex[host]
	return slug, ok
}

// publicPort extracts the port the visitor used from the request's Host
// header. Empty when the request was on a default port (80/443) so we can
// build URLs without a trailing :443. Falls back to s.port for safety.
func (s *Server) publicPort(c *echo.Context) string {
	if i := strings.LastIndex(c.Request().Host, ":"); i != -1 {
		return c.Request().Host[i:] // includes the leading ':'
	}
	return ""
}

// siteURL builds the public URL of a hosted site, using the same scheme/port
// the admin caller is on. https://slug.apps.jtarchie.com on Fly,
// http://slug.localhost:8080 in dev.
func (s *Server) siteURL(c *echo.Context, slug, path string) string {
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.Scheme() + "://" + slug + "." + s.domain + s.publicPort(c) + path
}

// adminURL builds an absolute URL on the main app domain. Used by the toolbar
// links injected into hosted-site pages on subdomains, where relative paths
// would point at the wrong host.
func (s *Server) adminURL(c *echo.Context, path string) string {
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.Scheme() + "://" + s.domain + s.publicPort(c) + path
}

// snapshotBefore wraps snapshot.Create with the "warn-only" policy used by
// every non-build mutation hook. Losing an undo point is not as bad as
// blocking the user's edit, so failures are logged and swallowed.
func (s *Server) snapshotBefore(ctx context.Context, slug, reason string) {
	if s.snapshot == nil {
		return
	}
	_, err := s.snapshot.Create(ctx, slug, reason)
	if err != nil {
		slog.Warn("snapshot.create_failed", "slug", slug, "reason", reason, "err", err)
	}
}

// startBuild kicks off the build via the build service and renders the
// progress page. SSE subscribers learn about the build through the events
// tracker.
func (s *Server) startBuild(c *echo.Context, p build.Params) error {
	s.build.Start(p)
	return s.render(c, "progress", map[string]any{
		"Slug":           p.Slug,
		"SiteURL":        s.siteURL(c, p.Slug, "/"),
		"PollIntervalMS": progressPollIntervalMS,
		"MaxChecks":      progressMaxChecks,
	})
}

func (s *Server) render(c *echo.Context, name string, data any) error {
	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, name, data)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "render "+name, err)
	}
	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
}

func httpErr(code int, msg string, err error) *echo.HTTPError {
	return echo.NewHTTPError(code, fmt.Sprintf("%s: %s", msg, err))
}

// notFound returns a real *echo.HTTPError so it survives the slog-echo
// middleware unchanged. The echo.Err* sentinels (echo.ErrNotFound &c.) are an
// unexported *httpError type that slog-echo's `err.(*echo.HTTPError)` check
// fails, after which slog-echo wraps them in a fresh 500 — turning every 404
// into an Internal Server Error in the response. Use this helper anywhere
// we'd reach for echo.ErrNotFound.
func notFound() *echo.HTTPError {
	return echo.NewHTTPError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
}

func (s *Server) landingHandler(c *echo.Context) error {
	return s.render(c, "landing", map[string]any{
		"Templates": templates.All(),
		"Domain":    s.domain,
	})
}

type appLink struct {
	Name        string
	Title       string
	Description string
	URL         string
}

func (s *Server) appsHandler(c *echo.Context) error {
	ctx := c.Request().Context()
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list apps", err)
	}

	links := make([]appLink, 0, len(apps))
	for _, app := range apps {
		meta := s.build.ReadMeta(ctx, app)
		links = append(links, appLink{
			Name:        app,
			Title:       meta.Title,
			Description: meta.Description,
			URL:         s.siteURL(c, app, "/"),
		})
	}
	sort.SliceStable(links, func(i, j int) bool {
		return appLinkKey(links[i]) < appLinkKey(links[j])
	})

	return s.render(c, "apps", links)
}

// appLinkKey orders apps by Title when present, otherwise by slug — keeps
// the listing readable as titles fill in over time without burying legacy
// (title-less) sites at the end.
func appLinkKey(a appLink) string {
	if a.Title != "" {
		return strings.ToLower(a.Title)
	}
	return strings.ToLower(a.Name)
}

// maxPromptBytes caps the user-supplied prompt on /build, /edit/:slug, and
// /edit/:slug/visual. Most real prompts are under a few hundred bytes; this is
// the field-level check that pairs with maxPromptBodyBytes on the route.
const maxPromptBytes = 4 * 1024

// maxPromptBodyBytes caps the entire request body on prompt-bearing POSTs.
// The handler-side check on maxPromptBytes still applies; this bounds the
// overall body so the hidden selection field and any other form data combined
// can't blow past a sane budget.
const maxPromptBodyBytes = 32 * 1024

func (s *Server) buildHandler(c *echo.Context) error {
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("prompt is too long (max %d bytes)", maxPromptBytes))
	}

	requested := strings.TrimSpace(c.FormValue("slug"))
	slug, err := s.resolveSlug(c.Request().Context(), requested)
	if err != nil {
		return err
	}

	tmpl := templates.Get(c.FormValue("template"))
	slog.Info("build.start", "slug", slug, "template", tmpl.ID)
	return s.startBuild(c, build.Params{
		Slug:         slug,
		Prompt:       prompt,
		LogKey:       "build",
		Template:     tmpl,
		SeedSkeleton: true,
	})
}

// resolveSlug returns either a validated user-provided slug or a freshly
// generated one. User-provided slugs are validated for shape and checked for
// collisions in S3.
func (s *Server) resolveSlug(ctx context.Context, requested string) (string, error) {
	if requested == "" {
		return newSlug(), nil
	}
	err := validateSlug(requested)
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	existing, err := s.store.List(ctx, requested)
	if err != nil {
		return "", httpErr(http.StatusInternalServerError, "check slug", err)
	}
	if len(existing) > 0 {
		return "", echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("slug %q is already taken", requested))
	}
	return requested, nil
}

func (s *Server) statusHandler(c *echo.Context) error {
	slug := c.Param("slug")
	status := s.events.Get(slug)
	if status == nil {
		status = &events.Status{Slug: slug, Status: "unknown"}
	}
	return c.JSON(http.StatusOK, status) //nolint:wrapcheck
}

// eventsHandler streams a slug's build events as SSE. It first replays any
// past events so a late connection still sees what happened, then forwards
// live events until the build hits a terminal status or the client
// disconnects.
func (s *Server) eventsHandler(c *echo.Context) error {
	slug := c.Param("slug")
	w := c.Response()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	history, sub, terminal := s.events.Subscribe(slug)
	if sub == nil {
		// No status known for this slug — emit a single "unknown" frame and bail.
		_ = writeSSE(w, events.Event{Type: events.TypeStatus, Status: "unknown", Time: time.Now()})
		flush()
		return nil
	}
	defer s.events.Unsubscribe(slug, sub)

	for _, e := range history {
		err := writeSSE(w, e)
		if err != nil {
			return nil //nolint:nilerr // client gone, just stop streaming
		}
	}
	flush()
	if terminal {
		return nil
	}

	ctx := c.Request().Context()
	for {
		select {
		case e, ok := <-sub:
			if !ok {
				return nil
			}
			err := writeSSE(w, e)
			if err != nil {
				return nil //nolint:nilerr
			}
			flush()
			if e.Type == events.TypeStatus && (e.Status == events.StatusCompleted || e.Status == events.StatusFailed) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func writeSSE(w io.Writer, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

func (s *Server) proxyHandler(c *echo.Context, slug string) error {
	ctx := c.Request().Context()

	reqPath := strings.TrimPrefix(c.Request().URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}

	candidates := []string{reqPath}
	if !strings.HasSuffix(reqPath, ".html") {
		candidates = append(candidates, reqPath+".html", reqPath+"/index.html")
	}

	for _, candidate := range candidates {
		obj, err := s.store.Read(ctx, slug, candidate)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "read object", err)
		}
		if obj.Content == "" {
			continue
		}

		c.Response().Header().Set("ETag", obj.ETag)
		setProxyCacheHeaders(c)

		if c.Request().Header.Get("If-None-Match") == obj.ETag {
			return c.NoContent(http.StatusNotModified) //nolint:wrapcheck
		}
		if match := c.Request().Header.Get("If-Match"); match != "" && match != obj.ETag {
			return c.NoContent(http.StatusPreconditionFailed) //nolint:wrapcheck
		}

		ct := resolveContentType(obj.ContentType, candidate)
		if strings.HasPrefix(ct, "text/html") {
			return c.HTML(http.StatusOK, s.injectEditToolbar(c, obj.Content, slug, candidate)) //nolint:wrapcheck
		}
		return c.Blob(http.StatusOK, ct, []byte(obj.Content)) //nolint:wrapcheck
	}

	return notFound()
}

// setProxyCacheHeaders picks cache headers for static-proxy responses based
// on whether we're serving the main app subdomain (admin previewing — always
// fresh) or a custom domain (cacheable, since a CDN sits in front).
func setProxyCacheHeaders(c *echo.Context) {
	h := c.Response().Header()
	if c.Get("custom_domain") == true {
		h.Set("Cache-Control", "public, max-age=300, s-maxage=3600")
		h.Set("Vary", "Accept-Encoding")
		return
	}
	h.Set("Cache-Control", "no-store")
}

// resolveContentType prefers the type recorded with the object. When that's
// missing or the legacy default (every pre-asset upload was stored as
// text/html), fall back to detecting from the file extension. Older sites
// have all files stamped text/html in S3, so the extension is the only
// signal for assets that were written via the agent's write_file tool —
// those are always HTML, so the default still works.
func resolveContentType(stored, name string) string {
	if stored != "" && stored != store.DefaultContentType {
		return stored
	}
	if ext := path.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	if stored != "" {
		return stored
	}
	return store.DefaultContentType
}

// injectEditToolbar inserts the edit toolbar before </body>. Skipped on
// custom-domain responses (so the CDN never caches the toolbar bytes) and
// when the visitor isn't an admin. If no </body> tag exists, the content is
// returned unchanged.
func (s *Server) injectEditToolbar(c *echo.Context, htmlContent, slug, page string) string {
	if c.Get("custom_domain") == true {
		return htmlContent
	}
	if !s.isAdmin(c) {
		return htmlContent
	}
	if !strings.Contains(htmlContent, "</body>") {
		return htmlContent
	}

	q := url.Values{"page": []string{page}}.Encode()
	editURL := s.adminURL(c, "/edit/"+slug) + "?" + q
	visualURL := s.adminURL(c, "/edit/"+slug+"/visual") + "?" + q

	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, "toolbar", struct {
		EditURL   template.URL
		VisualURL template.URL
	}{
		EditURL:   template.URL(editURL),   //nolint:gosec // URL built from controlled inputs above.
		VisualURL: template.URL(visualURL), //nolint:gosec // URL built from controlled inputs above.
	})
	if err != nil {
		slog.Warn("toolbar.render_failed", "slug", slug, "err", err)
		return htmlContent
	}
	return strings.Replace(htmlContent, "</body>", buf.String(), 1)
}

type editData struct {
	Slug      string
	SiteURL   string // root of the live site (no trailing path)
	PageURL   string // current page on the live site (for the iframe)
	Active    string // sub-nav highlight key; always "edit" for this handler
	Page      string
	Pages     []string
	Assets    []editAsset
	Functions []string
	// Flash is a transient banner key set via ?flash=... after a redirect.
	// Currently only "lint-clean" is used; the template ignores unknown keys.
	Flash string
}

// editAsset is the per-image row rendered on the edit page. Alt is shown
// next to the path so users can see what the captioner inferred without
// round-tripping through the agent.
type editAsset struct {
	Path string
	Alt  string
}

func validatePage(page string) error {
	if page == "" {
		return nil
	}
	if strings.Contains(page, "..") || strings.HasPrefix(page, "/") || strings.Contains(page, `\`) {
		return fmt.Errorf("invalid page %q", page)
	}
	return nil
}

// reservedSlugs collide with platform routes/hosts and cannot be used as
// site slugs.
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "edit": true, "apps": true,
	"status": true, "build": true, "events": true, "upload": true,
	"history": true, "settings": true, "test": true, "relint": true,
	"admin": true, "login": true, "logout": true,
}

func validateSlug(slug string) error {
	if len(slug) < 3 || len(slug) > 30 {
		return errors.New("slug must be 3-30 characters")
	}
	for i, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(slug)-1:
		default:
			return errors.New("slug must be lowercase letters, digits, and hyphens (no leading/trailing hyphen)")
		}
	}
	if reservedSlugs[slug] {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	return nil
}

func (s *Server) editHandler(c *echo.Context) error {
	slug := c.Param("slug")
	page := c.QueryParam("page")

	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	all, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list pages", err)
	}
	pages, assetPaths := build.SplitFilesByKind(all)

	assets := make([]editAsset, 0, len(assetPaths))
	for _, p := range assetPaths {
		row := editAsset{Path: p}
		// Reads are cached via ARC, so this is cheap on hot paths and a
		// one-time S3 round-trip on cold ones.
		obj, readErr := s.store.Read(ctx, slug, p)
		if readErr == nil && obj != nil {
			row.Alt = obj.Metadata["alt"]
		} else if readErr != nil {
			slog.Warn("edit.asset_meta", "slug", slug, "path", p, "err", readErr)
		}
		assets = append(assets, row)
	}

	functions := collectFunctionNames(all)

	return s.render(c, "edit", editData{
		Slug:      slug,
		Functions: functions,
		SiteURL:   s.siteURL(c, slug, "/"),
		PageURL:   s.siteURL(c, slug, "/"+page),
		Active:    "edit",
		Page:      page,
		Pages:     pages,
		Assets:    assets,
		Flash:     c.QueryParam("flash"),
	})
}

func (s *Server) editSubmitHandler(c *echo.Context) error {
	slug := c.Param("slug")
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	page := c.FormValue("page")
	selection := strings.TrimSpace(c.FormValue("selection"))

	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("prompt is too long (max %d bytes)", maxPromptBytes))
	}
	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if existing := s.events.Get(slug); existing != nil && existing.Status == events.StatusBuilding {
		return echo.NewHTTPError(http.StatusConflict, "edit already in progress for this site")
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	seeds := s.build.EditSeeds(ctx, slug, prompt)
	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection), "template", tmpl.ID, "seeds", len(seeds))
	return s.startBuild(c, build.Params{
		Slug:     slug,
		Prompt:   build.EditPrompt(prompt, page, selection),
		LogKey:   "edit",
		Template: tmpl,
		Seeds:    seeds,
	})
}

// relintHandler forces a fresh lint pass and, when issues are found, pushes
// them back to the agent for a fix-up build. On clean sites it redirects to
// the edit page with a flash banner — no LLM cycles spent.
func (s *Server) relintHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if existing := s.events.Get(slug); existing != nil && existing.Status == events.StatusBuilding {
		return echo.NewHTTPError(http.StatusConflict, "build already in progress for this site")
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	lintErrs := s.build.Lint(ctx, slug, tmpl)

	if len(lintErrs) == 0 {
		slog.Info("relint.clean", "slug", slug)
		return c.Redirect(http.StatusSeeOther, "/edit/"+slug+"?flash=lint-clean") //nolint:wrapcheck
	}

	slog.Info("relint.start", "slug", slug, "issues", len(lintErrs), "template", tmpl.ID)
	return s.startBuild(c, build.Params{
		Slug:     slug,
		Prompt:   build.LintFixPrompt(lintErrs),
		LogKey:   "relint",
		Template: tmpl,
	})
}

type settingsData struct {
	Slug             string
	SiteURL          string
	Active           string
	Domains          string
	DomainsError     string
	FunctionsEnabled bool
	FunctionsByTmpl  bool // the template already enables functions — checkbox is locked-on
	PublicAPIEnabled bool
}

func (s *Server) settingsHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	meta := s.build.ReadMeta(c.Request().Context(), slug)
	tmpl := build.EffectiveTemplate(meta)
	byTmpl := tmpl != nil && templates.Get(meta.Template) != nil && templates.Get(meta.Template).EnablesFunctions
	return s.render(c, "settings", settingsData{
		Slug:             slug,
		SiteURL:          s.siteURL(c, slug, "/"),
		Active:           "settings",
		Domains:          strings.Join(meta.Domains, "\n"),
		FunctionsEnabled: tmpl != nil && tmpl.EnablesFunctions,
		FunctionsByTmpl:  byTmpl,
		PublicAPIEnabled: meta.EnablesPublicAPI,
	})
}

func (s *Server) settingsSubmitHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)

	domains, derr := s.parseDomains(c.FormValue("domains"), slug)
	if derr != nil {
		return echo.NewHTTPError(http.StatusBadRequest, derr.Error())
	}
	meta.Domains = domains

	// Only honour the override when the template doesn't already enable
	// functions. Templates that do (contact-form, guestbook, tiny-shop) keep
	// functions on regardless of the per-site bit, so the form's checked-state
	// always matches what the user sees.
	if base := templates.Get(meta.Template); base == nil || !base.EnablesFunctions {
		meta.EnablesFunctions = c.FormValue("enable_functions") == "on"
	}

	meta.EnablesPublicAPI = c.FormValue("enable_public_api") == "on"

	s.snapshotBefore(ctx, slug, snapshot.ReasonSettings)

	err = s.build.WriteMeta(ctx, slug, meta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save settings", err)
	}
	s.rebuildDomainIndex(ctx)
	return c.Redirect(http.StatusSeeOther, "/settings/"+slug) //nolint:wrapcheck
}

// parseDomains splits the settings-form textarea into a deduped, normalized
// list of hostnames. Rejects entries that collide with the main app domain
// (or its subdomains) and ones already claimed by another slug.
func (s *Server) parseDomains(raw, owningSlug string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, err := build.NormalizeDomain(line)
		if err != nil {
			return nil, fmt.Errorf("normalize domain: %w", err)
		}
		if host == s.domain || strings.HasSuffix(host, "."+s.domain) {
			return nil, fmt.Errorf("domain %q overlaps the main app domain", host)
		}
		if other, ok := s.lookupCustomDomain(host); ok && other != owningSlug {
			return nil, fmt.Errorf("domain %q is already claimed by site %q", host, other)
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out, nil
}

const (
	maxUploadBytes  = 5 << 20 // 5 MiB
	uploadAssetsDir = "assets"
)

// allowedAssetTypes maps sniffed MIME types to a stable file extension we'll
// store under. Keep this restrictive — the agent only knows how to embed
// images via <img>, so we don't accept fonts/video/etc. yet.
var allowedAssetTypes = map[string]string{
	"image/jpeg":    ".jpg",
	"image/png":     ".png",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/svg+xml": ".svg",
}

type uploadResponse struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Alt         string `json:"alt,omitempty"`
	Description string `json:"description,omitempty"`
}

// captionTimeout caps how long the upload handler waits on the vision call
// before giving up and storing the asset without metadata. Local models can
// be slow; we'd rather have a usable upload than a hung POST.
const captionTimeout = 90 * time.Second

func (s *Server) uploadHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	header, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "file is required")
	}
	if header.Size > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	src, err := header.Open()
	if err != nil {
		return httpErr(http.StatusInternalServerError, "open upload", err)
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if len(body) > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	contentType := http.DetectContentType(body)
	contentType = strings.SplitN(contentType, ";", 2)[0]
	ext, ok := allowedAssetTypes[contentType]
	if !ok {
		// SVG sniffs as text/xml or text/plain; trust the extension when the
		// upload looks textual.
		if e := strings.ToLower(path.Ext(header.Filename)); e == ".svg" {
			contentType = "image/svg+xml"
			ext = ".svg"
			ok = true
		}
	}
	if !ok {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported type %q (allowed: jpeg, png, gif, webp, svg)", contentType))
	}

	name, err := safeAssetName(header.Filename, ext)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	relPath := uploadAssetsDir + "/" + name

	// Caption synchronously so the UI can show the suggested alt-text right
	// next to the upload, and so the agent's first list_assets call already
	// has the metadata. Failures here are non-fatal — the upload still lands.
	caption, captionErr := s.captionUpload(c.Request().Context(), body, contentType)
	if captionErr != nil {
		slog.Warn("upload.caption_failed", "slug", slug, "path", relPath, "err", captionErr)
	}

	metadata := map[string]string{}
	if caption.Alt != "" {
		metadata["alt"] = caption.Alt
	}
	if caption.Description != "" {
		metadata["description"] = caption.Description
	}

	s.snapshotBefore(c.Request().Context(), slug, snapshot.ReasonUpload)

	err = s.store.Write(c.Request().Context(), slug, relPath, string(body), contentType, metadata)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "store asset", err)
	}

	slog.Info("upload.done", "slug", slug, "path", relPath, "type", contentType, "size", len(body), "captioned", caption.Alt != "")
	return c.JSON(http.StatusOK, uploadResponse{ //nolint:wrapcheck
		Path:        relPath,
		URL:         fmt.Sprintf("http://%s.%s:%s/%s", slug, s.domain, s.port, relPath),
		ContentType: contentType,
		Size:        len(body),
		Alt:         caption.Alt,
		Description: caption.Description,
	})
}

// captionUpload runs the vision sub-agent under a bounded deadline so a slow
// or unresponsive model can't hold the upload request open. The returned
// caption is zero-valued on failure; callers must tolerate that.
func (s *Server) captionUpload(ctx context.Context, body []byte, contentType string) (agent.Caption, error) {
	cctx, cancel := context.WithTimeout(ctx, captionTimeout)
	defer cancel()
	caption, err := agent.CaptionAsset(cctx, s.llm, body, contentType)
	if err != nil {
		return caption, fmt.Errorf("caption asset: %w", err)
	}
	return caption, nil
}

// safeAssetName produces a filesystem-safe filename derived from the
// upload's basename, forcing the extension to match the sniffed content
// type. Anything outside [a-z0-9._-] becomes a dash; empty stems become
// "asset".
func safeAssetName(original, ext string) (string, error) {
	stem := strings.TrimSuffix(path.Base(original), path.Ext(original))
	stem = strings.ToLower(stem)
	var b strings.Builder
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.':
			// collapse dots so we don't end up with a/../ shenanigans
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		cleaned = "asset"
	}
	if len(cleaned) > 60 {
		cleaned = cleaned[:60]
	}
	return cleaned + ext, nil
}
