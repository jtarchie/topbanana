package main

import (
	"bytes"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/labstack/echo/v5"
	slogecho "github.com/samber/slog-echo"

	adkmodel "google.golang.org/adk/model"
)

type Server struct {
	store  *Store
	domain string
	port   string
	llm    adkmodel.LLM
	tpl    *template.Template
}

func NewServer(store *Store, domain, port string, llm adkmodel.LLM) *echo.Echo {
	s := &Server{store: store, domain: domain, port: port, llm: llm}

	tpl, err := template.New("apps.html").Parse(appsTemplate)
	if err != nil {
		panic(fmt.Sprintf("failed to parse apps template: %s", err))
	}
	s.tpl = tpl

	e := echo.New()
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	e.Use(slogecho.New(logger))
	e.Use(s.subdomainMiddleware())

	e.GET("/", s.landingHandler)
	e.POST("/build", s.buildHandler)
	e.GET("/apps", s.appsHandler)

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

	err := runAgent(c.Request().Context(), s.llm, s.store, slug, prompt)
	if err != nil {
		slog.Error("build.failed", "slug", slug, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("build failed: %s", err))
	}

	slog.Info("build.done", "slug", slug)
	target := fmt.Sprintf("http://%s.%s:%s", slug, s.domain, s.port)
	return c.Redirect(http.StatusFound, target) //nolint:wrapcheck
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

			return c.HTML(http.StatusOK, obj.Content) //nolint:wrapcheck
		}
	}

	return echo.ErrNotFound
}
