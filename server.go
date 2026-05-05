package main

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
	"regexp"
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

// BuildEvent is the payload streamed to /events/:slug subscribers and recorded
// in BuildStatus.Events for replay on reconnect.
type BuildEvent struct {
	Type    string    `json:"type"`              // "status" | "tool"
	Status  string    `json:"status,omitempty"`  // for type=status: building|completed|failed|linting|retry
	Tool    string    `json:"tool,omitempty"`    // for type=tool: write_file|read_file|list_files|list_assets
	Path    string    `json:"path,omitempty"`    // for type=tool: file path the tool acted on
	Message string    `json:"message,omitempty"` // optional human-readable detail (errors, retry reason)
	Time    time.Time `json:"time"`
}

type BuildStatus struct {
	Slug     string    `json:"slug"`
	Status   string    `json:"status"` // "building", "completed", "failed", "unknown"
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"-"` // set when Status flips to terminal; drives eviction

	// Events keeps the full event log so SSE subscribers can replay history when
	// they connect partway through a build. subs holds live subscriber channels.
	// Both fields are guarded by the parent buildTracker's mutex.
	Events []BuildEvent                 `json:"-"`
	subs   map[chan BuildEvent]struct{} `json:"-"`
}

type buildTracker struct {
	mu sync.Mutex
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
	b.emit(slug, BuildEvent{Type: "status", Status: "building"})
}

func (b *buildTracker) complete(slug string) {
	b.emit(slug, BuildEvent{Type: "status", Status: "completed"})
}

func (b *buildTracker) fail(slug string, err error) {
	b.emit(slug, BuildEvent{Type: "status", Status: "failed", Message: err.Error()})
}

// emit records an event on the slug's status, fans it out to any live SSE
// subscribers (dropping for slow consumers — they can use replay on reconnect),
// and updates the terminal Finished timestamp when the status reaches a final
// state. All mutation happens under b.mu so subscribe/unsubscribe stay race-free.
func (b *buildTracker) emit(slug string, event BuildEvent) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	s, ok := b.m[slug]
	if !ok {
		s = &BuildStatus{Slug: slug, subs: map[chan BuildEvent]struct{}{}}
		b.m[slug] = s
	}
	s.Events = append(s.Events, event)
	if event.Type == "status" {
		s.Status = event.Status
		switch event.Status {
		case "completed":
			s.Finished = event.Time
			s.Error = ""
		case "failed":
			s.Finished = event.Time
			s.Error = event.Message
		}
	}
	for sub := range s.subs {
		select {
		case sub <- event:
		default:
		}
	}
}

func (b *buildTracker) get(slug string) *BuildStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[slug]
	if !ok {
		return nil
	}
	// Copy the public fields; subs/Events stay internal.
	return &BuildStatus{Slug: s.Slug, Status: s.Status, Error: s.Error, Finished: s.Finished}
}

// subscribe returns a snapshot of past events plus a channel that receives new
// ones. terminal indicates the build already finished — callers should still
// drain the channel for any concurrent emits but can exit promptly.
func (b *buildTracker) subscribe(slug string) (history []BuildEvent, ch chan BuildEvent, terminal bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[slug]
	if !ok {
		return nil, nil, true
	}
	if s.subs == nil {
		s.subs = map[chan BuildEvent]struct{}{}
	}
	ch = make(chan BuildEvent, 64)
	s.subs[ch] = struct{}{}
	history = append([]BuildEvent(nil), s.Events...)
	terminal = !s.Finished.IsZero()
	return history, ch, terminal
}

func (b *buildTracker) unsubscribe(slug string, ch chan BuildEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.m[slug]
	if !ok {
		return
	}
	if _, alive := s.subs[ch]; !alive {
		return // sweep got there first and already closed it
	}
	delete(s.subs, ch)
	close(ch)
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
// rather than silently disappearing state. Subscribers attached to evicted entries
// have their channels closed so their goroutines can exit.
func (b *buildTracker) sweep(now time.Time) {
	cutoff := now.Add(-terminalEntryTTL)
	b.mu.Lock()
	defer b.mu.Unlock()
	for slug, s := range b.m {
		if !s.Finished.IsZero() && s.Finished.Before(cutoff) {
			for ch := range s.subs {
				close(ch)
			}
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
	e.GET("/events/:slug", s.eventsHandler)
	e.GET("/apps", s.appsHandler)
	e.GET("/edit/:slug", s.editHandler)
	e.POST("/edit/:slug", s.editSubmitHandler)
	e.POST("/upload/:slug", s.uploadHandler)

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

func (s *Server) buildAndLint(ctx context.Context, slug, prompt string, tmpl *SiteTemplate, seeds []seedToolCall) error {
	emit := func(e BuildEvent) { s.builds.emit(slug, e) }

	err := runAgent(ctx, s.llm, s.store, slug, prompt, tmpl, seeds, emit)
	if err != nil {
		return err
	}

	for attempt := 0; attempt <= maxLintRetries; attempt++ {
		emit(BuildEvent{Type: "status", Status: "linting"})
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
		emit(BuildEvent{Type: "status", Status: "retry", Message: fmt.Sprintf("fixing %d issue(s)", len(lintErrs))})
		fixPrompt := "Fix these issues in the site:\n" + strings.Join(msgs, "\n")
		err := runAgent(ctx, s.llm, s.store, slug, fixPrompt, tmpl, nil, emit)
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
func (s *Server) startBuild(c *echo.Context, slug, prompt, logKey string, tmpl *SiteTemplate, seedSkeleton bool, seeds []seedToolCall) error {
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
		err := s.buildAndLint(ctx, slug, prompt, tmpl, seeds)
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
		err := s.store.Write(ctx, slug, path, content, "text/html; charset=utf-8", nil)
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
	err = s.store.Write(ctx, slug, siteMetaFile, string(body), "application/json", nil)
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
		"Domain":    s.domain,
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

	requested := strings.TrimSpace(c.FormValue("slug"))
	slug, err := s.resolveSlug(c.Request().Context(), requested)
	if err != nil {
		return err
	}

	tmpl := GetSiteTemplate(c.FormValue("template"))
	slog.Info("build.start", "slug", slug, "template", tmpl.ID)
	return s.startBuild(c, slug, prompt, "build", tmpl, true, nil)
}

// resolveSlug returns either a validated user-provided slug or a freshly generated one.
// User-provided slugs are validated for shape and checked for collisions in S3.
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
	status := s.builds.get(slug)
	if status == nil {
		status = &BuildStatus{Slug: slug, Status: "unknown"}
	}
	return c.JSON(http.StatusOK, status) //nolint:wrapcheck
}

// eventsHandler streams a slug's build events as SSE. It first replays any
// past events so a late connection still sees what happened, then forwards
// live events until the build hits a terminal status or the client disconnects.
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

	history, sub, terminal := s.builds.subscribe(slug)
	if sub == nil {
		// No status known for this slug — emit a single "unknown" frame and bail.
		_ = writeSSE(w, BuildEvent{Type: "status", Status: "unknown", Time: time.Now()})
		flush()
		return nil
	}
	defer s.builds.unsubscribe(slug, sub)

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
			if e.Type == "status" && (e.Status == "completed" || e.Status == "failed") {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}

func writeSSE(w io.Writer, event BuildEvent) error {
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

		ct := resolveContentType(obj.ContentType, candidate)
		if strings.HasPrefix(ct, "text/html") {
			return c.HTML(http.StatusOK, s.injectEditToolbar(obj.Content, slug, candidate)) //nolint:wrapcheck
		}
		return c.Blob(http.StatusOK, ct, []byte(obj.Content)) //nolint:wrapcheck
	}

	return echo.ErrNotFound
}

// resolveContentType prefers the type recorded with the object. When that's
// missing or the legacy default (every pre-asset upload was stored as text/html),
// fall back to detecting from the file extension. Older sites have all files
// stamped text/html in S3, so the extension is the only signal for assets that
// were written via the agent's write_file tool — those are always HTML, so the
// default still works.
func resolveContentType(stored, name string) string {
	if stored != "" && stored != defaultContentType {
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
	return defaultContentType
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
	Assets []editAsset
}

// editAsset is the per-image row rendered on the edit page. Alt is shown next
// to the path so users can see what the captioner inferred without round-tripping
// through the agent.
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

// reservedSlugs collide with platform routes/hosts and cannot be used as site slugs.
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "edit": true, "apps": true,
	"status": true, "build": true, "events": true, "upload": true,
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
	pages, assetPaths := splitFilesByKind(all)

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

	return s.render(c, "edit", editData{
		Slug:   slug,
		Domain: s.domain,
		Port:   s.port,
		Page:   page,
		Pages:  pages,
		Assets: assets,
	})
}

// splitFilesByKind partitions a slug's file list into editable HTML pages
// versus uploaded assets. Sidecars and unknown files are dropped from both.
func splitFilesByKind(files []string) (pages, assets []string) {
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, "."):
			// sidecars like .buildabear.json
		case strings.HasPrefix(f, "assets/"):
			assets = append(assets, f)
		case strings.HasSuffix(f, ".html"):
			pages = append(pages, f)
		}
	}
	return pages, assets
}

// editPrefetchTotalCap caps the total bytes of HTML page content we'll inline
// into seeded read_file responses. Beyond this, we let the agent issue its own
// read_file calls so we don't blow the context window on a sprawling site.
const editPrefetchTotalCap = 32 * 1024

// editSeeds returns synthetic tool-call seeds for an edit invocation: always a
// list_files seed (so the agent doesn't need that round-trip), and a read_file
// seed for each existing HTML page mentioned by name (full filename or
// basename) in the user's prompt, capped at editPrefetchTotalCap total bytes.
//
// All errors are swallowed and logged: seeding is an optimization, never a
// gating step. If we can't list the bucket, we return nil seeds and the agent
// proceeds as before.
func (s *Server) editSeeds(ctx context.Context, slug, prompt string) []seedToolCall {
	files, err := s.store.List(ctx, slug)
	if err != nil {
		slog.Warn("edit.seeds.list_failed", "slug", slug, "err", err)
		return nil
	}
	pages, _ := splitFilesByKind(files)
	if len(pages) == 0 {
		return nil
	}

	seeds := make([]seedToolCall, 0, 1+len(pages))
	seeds = append(seeds, seedToolCall{
		Name:     "list_files",
		Args:     map[string]any{},
		Response: map[string]any{"files": pages},
	})

	matched := pagesNamedInPrompt(pages, prompt)
	total := 0
	capped := false
	for _, page := range matched {
		obj, err := s.store.Read(ctx, slug, page)
		if err != nil || obj == nil {
			slog.Warn("edit.seeds.read_failed", "slug", slug, "page", page, "err", err)
			continue
		}
		if total+len(obj.Content) > editPrefetchTotalCap {
			capped = true
			break
		}
		total += len(obj.Content)
		seeds = append(seeds, seedToolCall{
			Name:     "read_file",
			Args:     map[string]any{"path": page},
			Response: map[string]any{"content": obj.Content},
		})
	}

	slog.Info("edit.prefetch", "slug", slug, "pages", len(pages), "matched", len(matched), "seeded_reads", len(seeds)-1, "bytes", total, "capped", capped)
	return seeds
}

// pagesNamedInPrompt returns the subset of pages whose full name (e.g.
// "about.html") or basename (e.g. "about") appears as a whole word in prompt.
// The candidate set is built from the actual file list, so a stray "home" in
// prose only matches when home.html truly exists.
func pagesNamedInPrompt(pages []string, prompt string) []string {
	if len(pages) == 0 || prompt == "" {
		return nil
	}

	tokens := make([]string, 0, 2*len(pages))
	byToken := make(map[string]string, 2*len(pages))
	for _, p := range pages {
		base := strings.TrimSuffix(p, ".html")
		for _, t := range []string{p, base} {
			lower := strings.ToLower(t)
			if _, seen := byToken[lower]; seen {
				continue
			}
			byToken[lower] = p
			tokens = append(tokens, regexp.QuoteMeta(lower))
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	re, err := regexp.Compile(`(?i)\b(?:` + strings.Join(tokens, "|") + `)\b`)
	if err != nil {
		slog.Warn("edit.seeds.regex_failed", "err", err)
		return nil
	}

	seen := make(map[string]bool, len(pages))
	out := make([]string, 0, len(pages))
	for _, m := range re.FindAllString(prompt, -1) {
		page, ok := byToken[strings.ToLower(m)]
		if !ok || seen[page] {
			continue
		}
		seen[page] = true
		out = append(out, page)
	}
	return out
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

	ctx := c.Request().Context()
	meta := s.readSiteMeta(ctx, slug)
	tmpl := GetSiteTemplate(meta.Template)
	seeds := s.editSeeds(ctx, slug, prompt)
	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection), "template", tmpl.ID, "seeds", len(seeds))
	return s.startBuild(c, slug, buildEditPrompt(prompt, page, selection), "edit", tmpl, false, seeds)
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
		// SVG sniffs as text/xml or text/plain; trust the extension when the upload looks textual.
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
func (s *Server) captionUpload(ctx context.Context, body []byte, contentType string) (AssetCaption, error) {
	cctx, cancel := context.WithTimeout(ctx, captionTimeout)
	defer cancel()
	return captionAsset(cctx, s.llm, body, contentType)
}

// safeAssetName produces a filesystem-safe filename derived from the upload's
// basename, forcing the extension to match the sniffed content type. Anything
// outside [a-z0-9._-] becomes a dash; empty stems become "asset".
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

func buildEditPrompt(prompt, page, selection string) string {
	switch {
	case page == "":
		return "You are editing an existing multi-page site. Apply the user's change by editing the existing files in place — do not rewrite pages from scratch and do not delete content the user did not ask you to remove.\n\nUser prompt:\n" + prompt
	case selection == "":
		return fmt.Sprintf("Edit only the page '%s'. Use read_file to see current content first.\n\n%s", page, prompt)
	default:
		return fmt.Sprintf("In page '%s', the user selected this content:\n\n```html\n%s\n```\n\nApply this instruction to that selection (use read_file first to see the surrounding context):\n%s", page, selection, prompt)
	}
}
