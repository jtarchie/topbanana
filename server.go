package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/labstack/echo/v5"
	slogecho "github.com/samber/slog-echo"

	adkmodel "google.golang.org/adk/model"
)

type BuildStatus struct {
	Slug   string `json:"slug"`
	Status string `json:"status"` // "building", "completed", "failed"
	Error  string `json:"error,omitempty"`
}

type Server struct {
	store         *Store
	domain        string
	port          string
	llm           adkmodel.LLM
	tpl           *template.Template
	progressTpl   *template.Template
	editTpl       *template.Template
	buildStatusMu sync.RWMutex
	buildStatuses map[string]*BuildStatus
}

func NewServer(store *Store, domain, port string, llm adkmodel.LLM) *echo.Echo {
	s := &Server{
		store:         store,
		domain:        domain,
		port:          port,
		llm:           llm,
		buildStatuses: make(map[string]*BuildStatus),
	}

	tpl, err := template.New("apps.html").Parse(appsTemplate)
	if err != nil {
		panic(fmt.Sprintf("failed to parse apps template: %s", err))
	}
	s.tpl = tpl

	progressTpl, err := template.ParseFiles("templates/progress.html")
	if err != nil {
		panic(fmt.Sprintf("failed to parse progress template: %s", err))
	}
	s.progressTpl = progressTpl

	editTpl, err := template.New("edit.html").Parse(editTemplate)
	if err != nil {
		panic(fmt.Sprintf("failed to parse edit template: %s", err))
	}
	s.editTpl = editTpl

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
// Requests to the main domain (or localhost) fall through to normal routes.
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			host := c.Request().Host
			if i := strings.LastIndex(host, ":"); i != -1 {
				host = host[:i]
			}

			if host == s.domain || host == "localhost" || host == "127.0.0.1" || host == "0.0.0.0" {
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

const maxLintRetries = 3

func (s *Server) buildAndLint(ctx context.Context, slug, prompt string) error {
	err := runAgent(ctx, s.llm, s.store, slug, prompt)
	if err != nil {
		return err
	}

	for range maxLintRetries {
		lintErrs := lintApp(ctx, s.store, slug)
		if len(lintErrs) == 0 {
			return nil
		}

		msgs := make([]string, 0, len(lintErrs))
		for _, e := range lintErrs {
			msgs = append(msgs, e.Error())
		}
		fixPrompt := "Fix these issues in the site:\n" + strings.Join(msgs, "\n")
		slog.Info("build.lint_retry", "slug", slug, "issues", len(lintErrs))

		err := runAgent(ctx, s.llm, s.store, slug, fixPrompt)
		if err != nil {
			return err
		}
	}

	// Final lint check after retries
	lintErrs := lintApp(ctx, s.store, slug)
	if len(lintErrs) > 0 {
		msgs := make([]string, 0, len(lintErrs))
		for _, e := range lintErrs {
			msgs = append(msgs, e.Error())
		}
		return fmt.Errorf("lint errors after %d retries: %s", maxLintRetries, strings.Join(msgs, "; "))
	}

	return nil
}

func (s *Server) landingHandler(c *echo.Context) error {
	return c.HTML(http.StatusOK, landingPage) //nolint:wrapcheck
}

func (s *Server) appsHandler(c *echo.Context) error {
	ctx := c.Request().Context()
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to list apps: %s", err))
	}

	type appLink struct {
		Name string
		URL  string
	}
	var links []appLink
	for _, app := range apps {
		url := fmt.Sprintf("http://%s.%s:%s/", app, s.domain, s.port)
		links = append(links, appLink{Name: app, URL: url})
	}

	var buf bytes.Buffer

	err = s.tpl.Execute(&buf, links)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to render apps template: %s", err))
	}

	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
}

func (s *Server) buildHandler(c *echo.Context) error {
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}

	slug := newSlug()
	slog.Info("build.start", "slug", slug)

	// Initialize build status
	s.buildStatusMu.Lock()
	s.buildStatuses[slug] = &BuildStatus{
		Slug:   slug,
		Status: "building",
	}
	s.buildStatusMu.Unlock()

	// Run build asynchronously
	go func() {
		ctx := context.Background()
		err := s.buildAndLint(ctx, slug, prompt)
		if err != nil {
			slog.Error("build.failed", "slug", slug, "err", err)
			s.buildStatusMu.Lock()
			s.buildStatuses[slug] = &BuildStatus{Slug: slug, Status: "failed", Error: err.Error()}
			s.buildStatusMu.Unlock()
			return
		}
		slog.Info("build.done", "slug", slug)
		s.buildStatusMu.Lock()
		s.buildStatuses[slug] = &BuildStatus{Slug: slug, Status: "completed"}
		s.buildStatusMu.Unlock()
	}()

	// Render progress page
	progressData := map[string]string{
		"Slug":   slug,
		"Domain": s.domain,
		"Port":   s.port,
	}

	var buf bytes.Buffer
	err := s.progressTpl.Execute(&buf, progressData)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to render progress template: %s", err))
	}

	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
}

func (s *Server) statusHandler(c *echo.Context) error {
	slug := c.Param("slug")

	s.buildStatusMu.RLock()
	status, exists := s.buildStatuses[slug]
	s.buildStatusMu.RUnlock()

	if !exists {
		//nolint:wrapcheck
		return c.JSON(http.StatusNotFound, BuildStatus{
			Slug:   slug,
			Status: "unknown",
		})
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
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if obj.Content != "" {
			c.Response().Header().Set("ETag", obj.ETag)
			c.Response().Header().Set("Cache-Control", "public, max-age=3600")

			if c.Request().Header.Get("If-None-Match") == obj.ETag {
				return c.NoContent(http.StatusNotModified) //nolint:wrapcheck
			}

			if match := c.Request().Header.Get("If-Match"); match != "" && match != obj.ETag {
				return c.NoContent(http.StatusPreconditionFailed) //nolint:wrapcheck
			}

			content := injectEditToolbar(obj.Content, s.domain, s.port, slug, candidate)
			return c.HTML(http.StatusOK, content) //nolint:wrapcheck
		}
	}

	return echo.ErrNotFound
}

const editToolbarTpl = `<style>
#_bab{position:fixed;bottom:1rem;right:1rem;z-index:9999;background:rgba(0,0,0,.8);color:#fff;padding:.5rem 1rem;border-radius:4px;font:14px sans-serif}
#_bab a{color:#fff;text-decoration:none}
.in-frame #_bab{display:none}
</style>
<div id="_bab"><a target="_top" href="http://%s:%s/edit/%s?page=%s">&#x270e; Edit this page</a></div>
<script>
if(self!==top)document.body.classList.add('in-frame');
document.addEventListener('selectionchange',function(){
var s=getSelection();
if(!s||!s.rangeCount||!s.toString().trim())return;
var d=document.createElement('div');
d.appendChild(s.getRangeAt(0).cloneContents());
parent.postMessage({type:'_bab_sel',html:d.innerHTML,text:s.toString()},'*');
});
</script>
</body>`

func injectEditToolbar(html, domain, port, slug, path string) string {
	if !strings.Contains(html, "</body>") {
		return html
	}
	toolbar := fmt.Sprintf(editToolbarTpl,
		domain,
		port,
		url.PathEscape(slug),
		url.QueryEscape(path),
	)
	return strings.Replace(html, "</body>", toolbar, 1)
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

	ctx := c.Request().Context()
	pages, err := s.store.List(ctx, slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to list pages: %s", err))
	}

	var buf bytes.Buffer
	err = s.editTpl.Execute(&buf, editData{
		Slug:   slug,
		Domain: s.domain,
		Port:   s.port,
		Page:   page,
		Pages:  pages,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to render edit template: %s", err))
	}

	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
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

	s.buildStatusMu.RLock()
	existing := s.buildStatuses[slug]
	s.buildStatusMu.RUnlock()
	if existing != nil && existing.Status == "building" {
		return echo.NewHTTPError(http.StatusConflict, "edit already in progress for this site")
	}

	var fullPrompt string
	switch {
	case page == "":
		fullPrompt = prompt
	case selection == "":
		fullPrompt = fmt.Sprintf("Edit only the page '%s'. Use read_file to see current content first.\n\n%s", page, prompt)
	default:
		fullPrompt = fmt.Sprintf("In page '%s', the user selected this content:\n\n```html\n%s\n```\n\nApply this instruction to that selection (use read_file first to see the surrounding context):\n%s", page, selection, prompt)
	}

	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection))

	s.buildStatusMu.Lock()
	s.buildStatuses[slug] = &BuildStatus{Slug: slug, Status: "building"}
	s.buildStatusMu.Unlock()

	go func() {
		ctx := context.Background()
		err := s.buildAndLint(ctx, slug, fullPrompt)
		if err != nil {
			slog.Error("edit.failed", "slug", slug, "err", err)
			s.buildStatusMu.Lock()
			s.buildStatuses[slug] = &BuildStatus{Slug: slug, Status: "failed", Error: err.Error()}
			s.buildStatusMu.Unlock()
			return
		}
		slog.Info("edit.done", "slug", slug)
		s.buildStatusMu.Lock()
		s.buildStatuses[slug] = &BuildStatus{Slug: slug, Status: "completed"}
		s.buildStatusMu.Unlock()
	}()

	progressData := map[string]string{
		"Slug":   slug,
		"Domain": s.domain,
		"Port":   s.port,
	}

	var buf bytes.Buffer
	err = s.progressTpl.Execute(&buf, progressData)
	if err != nil {
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("failed to render progress template: %s", err))
	}

	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
}
