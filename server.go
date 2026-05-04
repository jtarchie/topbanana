package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v5"
	slogecho "github.com/samber/slog-echo"

	adkmodel "google.golang.org/adk/model"
)

const (
	maxLintRetries         = 3
	progressPollIntervalMS = 2000
	progressMaxChecks      = 180

	// terminalEntryTTL is how long a completed/failed BuildStatus stays in the tracker
	// after termination. The progress page polls every progressPollIntervalMS, so this
	// must be comfortably larger than that to guarantee the redirect is observed.
	terminalEntryTTL = time.Minute
	sweepInterval    = time.Minute
)

// siteMetaFile holds the per-site sidecar (template id, creation time). Stored
// alongside the HTML files in the same S3 prefix so it travels with the site.
const siteMetaFile = ".buildabear.json"

type siteMeta struct {
	Template string    `json:"template"`
	Created  time.Time `json:"created"`
}

type BuildStatus struct {
	Slug     string    `json:"slug"`
	Status   string    `json:"status"` // "building", "completed", "failed", "unknown"
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"-"` // set when Status flips to terminal; drives eviction
}

type buildTracker struct {
	mu sync.RWMutex
	m  map[string]*BuildStatus
}

// newBuildTracker spawns a background sweep goroutine that lives for the lifetime of
// the process; we don't bother with shutdown coordination because the only consumer
// is the long-running HTTP server.
func newBuildTracker() *buildTracker {
	b := &buildTracker{m: make(map[string]*BuildStatus)}
	go b.sweepLoop()
	return b
}

func (b *buildTracker) start(slug string) {
	b.set(&BuildStatus{Slug: slug, Status: "building"})
}

func (b *buildTracker) complete(slug string) {
	b.set(&BuildStatus{Slug: slug, Status: "completed", Finished: time.Now()})
}

func (b *buildTracker) fail(slug string, err error) {
	b.set(&BuildStatus{Slug: slug, Status: "failed", Error: err.Error(), Finished: time.Now()})
}

func (b *buildTracker) set(s *BuildStatus) {
	b.mu.Lock()
	b.m[s.Slug] = s
	b.mu.Unlock()
}

func (b *buildTracker) get(slug string) *BuildStatus {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.m[slug]
}

func (b *buildTracker) sweepLoop() {
	t := time.NewTicker(sweepInterval)
	defer t.Stop()
	for now := range t.C {
		b.sweep(now)
	}
}

// sweep removes terminal entries older than terminalEntryTTL. "building" entries are
// never swept — a hung agent is a separate problem, surfaced as a stuck progress page
// rather than silently disappearing state.
func (b *buildTracker) sweep(now time.Time) {
	cutoff := now.Add(-terminalEntryTTL)
	b.mu.Lock()
	defer b.mu.Unlock()
	for slug, s := range b.m {
		if !s.Finished.IsZero() && s.Finished.Before(cutoff) {
			delete(b.m, slug)
		}
	}
}

type Server struct {
	store  *Store
	domain string
	port   string
	llm    adkmodel.LLM
	tpl    *template.Template
	builds *buildTracker
}

// fallThroughHosts are hosts that should bypass subdomain proxying and hit the main routes.
var fallThroughHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"0.0.0.0":   true,
}

func NewServer(store *Store, domain, port string, llm adkmodel.LLM) *echo.Echo {
	tpl := template.New("")
	// layout.html defines shared partials (e.g. "head") used by the platform pages
	// below. It must be parsed first so the others can reference its blocks.
	template.Must(tpl.Parse(layoutTemplate))
	for _, t := range []struct{ name, body string }{
		{"landing", landingTemplate},
		{"apps", appsTemplate},
		{"progress", progressTemplate},
		{"edit", editTemplate},
		{"toolbar", editToolbarTemplate},
	} {
		template.Must(tpl.New(t.name).Parse(t.body))
	}

	s := &Server{
		store:  store,
		domain: domain,
		port:   port,
		llm:    llm,
		tpl:    tpl,
		builds: newBuildTracker(),
	}

	e := echo.New()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	e.Use(slogecho.New(logger))
	e.Use(s.subdomainMiddleware())

	e.GET("/", s.landingHandler)
	e.POST("/build", s.buildHandler)
	e.GET("/status/:slug", s.statusHandler)
	e.GET("/apps", s.appsHandler)
	e.GET("/edit/:slug", s.editHandler)
	e.POST("/edit/:slug", s.editSubmitHandler)

	return e
}

// subdomainMiddleware intercepts requests to *.domain and proxies them to S3.
// Requests to the main domain (or loopback) fall through to normal routes.
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			host := c.Request().Host
			if i := strings.LastIndex(host, ":"); i != -1 {
				host = host[:i]
			}

			if host == s.domain || fallThroughHosts[host] {
				return next(c)
			}

			slug, isSubdomain := strings.CutSuffix(host, "."+s.domain)
			if !isSubdomain {
				return next(c)
			}

			return s.proxyHandler(c, slug)
		}
	}
}

func (s *Server) buildAndLint(ctx context.Context, slug, prompt string, tmpl *SiteTemplate) error {
	err := runAgent(ctx, s.llm, s.store, slug, prompt, tmpl)
	if err != nil {
		return err
	}

	for attempt := 0; attempt <= maxLintRetries; attempt++ {
		lintErrs := lintApp(ctx, s.store, slug, tmpl)
		if len(lintErrs) == 0 {
			return nil
		}

		msgs := make([]string, 0, len(lintErrs))
		for _, e := range lintErrs {
			msgs = append(msgs, e.Error())
		}

		if attempt == maxLintRetries {
			return fmt.Errorf("lint errors after %d retries: %s", maxLintRetries, strings.Join(msgs, "; "))
		}

		slog.Info("build.lint_retry", "slug", slug, "attempt", attempt+1, "issues", len(lintErrs))
		fixPrompt := "Fix these issues in the site:\n" + strings.Join(msgs, "\n")
		err := runAgent(ctx, s.llm, s.store, slug, fixPrompt, tmpl)
		if err != nil {
			return err
		}
	}

	return nil
}

// startBuild seeds build status, runs buildAndLint asynchronously, and renders the progress page.
// logKey distinguishes "build" vs "edit" in slog output. When seedSkeleton is true (initial
// builds only), the template's skeleton files and metadata sidecar are written before the
// agent runs.
func (s *Server) startBuild(c *echo.Context, slug, prompt, logKey string, tmpl *SiteTemplate, seedSkeleton bool) error {
	s.builds.start(slug)

	go func() {
		ctx := context.Background()
		if seedSkeleton {
			err := s.seedTemplate(ctx, slug, tmpl)
			if err != nil {
				slog.Error(logKey+".seed_failed", "slug", slug, "template", tmpl.ID, "err", err)
				s.builds.fail(slug, err)
				return
			}
		}
		err := s.buildAndLint(ctx, slug, prompt, tmpl)
		if err != nil {
			slog.Error(logKey+".failed", "slug", slug, "err", err)
			s.builds.fail(slug, err)
			return
		}
		slog.Info(logKey+".done", "slug", slug)
		s.builds.complete(slug)
	}()

	return s.render(c, "progress", map[string]any{
		"Slug":           slug,
		"Domain":         s.domain,
		"Port":           s.port,
		"PollIntervalMS": progressPollIntervalMS,
		"MaxChecks":      progressMaxChecks,
	})
}

// seedTemplate writes the template's skeleton files (if any) and the
// .buildabear.json sidecar recording the template id. The sidecar lets later
// edits re-apply the same template addendum.
func (s *Server) seedTemplate(ctx context.Context, slug string, tmpl *SiteTemplate) error {
	if tmpl == nil {
		return nil
	}
	for path, content := range tmpl.Skeleton {
		err := s.store.Write(ctx, slug, path, content)
		if err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}
		slog.Info("template.seed", "slug", slug, "template", tmpl.ID, "path", path)
	}

	meta := siteMeta{Template: tmpl.ID, Created: time.Now().UTC()}
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode site meta: %w", err)
	}
	err = s.store.Write(ctx, slug, siteMetaFile, string(body))
	if err != nil {
		return fmt.Errorf("write site meta: %w", err)
	}
	return nil
}

// readSiteMeta returns the recorded template id for an existing site, or an
// empty value if the sidecar is missing (older sites pre-date templates).
func (s *Server) readSiteMeta(ctx context.Context, slug string) siteMeta {
	obj, err := s.store.Read(ctx, slug, siteMetaFile)
	if err != nil {
		slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
		return siteMeta{}
	}
	if obj.Content == "" {
		return siteMeta{}
	}
	var m siteMeta
	err = json.Unmarshal([]byte(obj.Content), &m)
	if err != nil {
		slog.Warn("site_meta.decode_failed", "slug", slug, "err", err)
		return siteMeta{}
	}
	return m
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

func (s *Server) landingHandler(c *echo.Context) error {
	return s.render(c, "landing", map[string]any{
		"Templates": AllSiteTemplates(),
	})
}

type appLink struct {
	Name string
	URL  string
}

func (s *Server) appsHandler(c *echo.Context) error {
	apps, err := s.store.ListApps(c.Request().Context())
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list apps", err)
	}

	links := make([]appLink, 0, len(apps))
	for _, app := range apps {
		links = append(links, appLink{
			Name: app,
			URL:  fmt.Sprintf("http://%s.%s:%s/", app, s.domain, s.port),
		})
	}

	return s.render(c, "apps", links)
}

func (s *Server) buildHandler(c *echo.Context) error {
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}

	tmpl := GetSiteTemplate(c.FormValue("template"))
	slug := newSlug()
	slog.Info("build.start", "slug", slug, "template", tmpl.ID)
	return s.startBuild(c, slug, prompt, "build", tmpl, true)
}

func (s *Server) statusHandler(c *echo.Context) error {
	slug := c.Param("slug")
	status := s.builds.get(slug)
	if status == nil {
		status = &BuildStatus{Slug: slug, Status: "unknown"}
	}
	return c.JSON(http.StatusOK, status) //nolint:wrapcheck
}

func (s *Server) proxyHandler(c *echo.Context, slug string) error {
	ctx := c.Request().Context()

	path := strings.TrimPrefix(c.Request().URL.Path, "/")
	if path == "" {
		path = "index.html"
	}

	candidates := []string{path}
	if !strings.HasSuffix(path, ".html") {
		candidates = append(candidates, path+".html", path+"/index.html")
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
		c.Response().Header().Set("Cache-Control", "public, max-age=3600")

		if c.Request().Header.Get("If-None-Match") == obj.ETag {
			return c.NoContent(http.StatusNotModified) //nolint:wrapcheck
		}
		if match := c.Request().Header.Get("If-Match"); match != "" && match != obj.ETag {
			return c.NoContent(http.StatusPreconditionFailed) //nolint:wrapcheck
		}

		return c.HTML(http.StatusOK, s.injectEditToolbar(obj.Content, slug, candidate)) //nolint:wrapcheck
	}

	return echo.ErrNotFound
}

// injectEditToolbar inserts the edit toolbar before </body>. If no </body> tag exists,
// the content is returned unchanged.
func (s *Server) injectEditToolbar(htmlContent, slug, page string) string {
	if !strings.Contains(htmlContent, "</body>") {
		return htmlContent
	}

	editURL := (&url.URL{
		Scheme:   "http",
		Host:     s.domain + ":" + s.port,
		Path:     "/edit/" + slug,
		RawQuery: url.Values{"page": []string{page}}.Encode(),
	}).String()

	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, "toolbar", struct{ EditURL template.URL }{
		EditURL: template.URL(editURL), //nolint:gosec // URL built from controlled inputs above.
	})
	if err != nil {
		slog.Warn("toolbar.render_failed", "slug", slug, "err", err)
		return htmlContent
	}
	return strings.Replace(htmlContent, "</body>", buf.String(), 1)
}

type editData struct {
	Slug   string
	Domain string
	Port   string
	Page   string
	Pages  []string
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

func (s *Server) editHandler(c *echo.Context) error {
	slug := c.Param("slug")
	page := c.QueryParam("page")

	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	pages, err := s.store.List(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list pages", err)
	}

	return s.render(c, "edit", editData{
		Slug:   slug,
		Domain: s.domain,
		Port:   s.port,
		Page:   page,
		Pages:  pages,
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
	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if existing := s.builds.get(slug); existing != nil && existing.Status == "building" {
		return echo.NewHTTPError(http.StatusConflict, "edit already in progress for this site")
	}

	meta := s.readSiteMeta(c.Request().Context(), slug)
	tmpl := GetSiteTemplate(meta.Template)
	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection), "template", tmpl.ID)
	return s.startBuild(c, slug, buildEditPrompt(prompt, page, selection), "edit", tmpl, false)
}

func buildEditPrompt(prompt, page, selection string) string {
	switch {
	case page == "":
		return prompt
	case selection == "":
		return fmt.Sprintf("Edit only the page '%s'. Use read_file to see current content first.\n\n%s", page, prompt)
	default:
		return fmt.Sprintf("In page '%s', the user selected this content:\n\n```html\n%s\n```\n\nApply this instruction to that selection (use read_file first to see the surrounding context):\n%s", page, selection, prompt)
	}
}
