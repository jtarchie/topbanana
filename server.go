package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	adkmodel "google.golang.org/adk/model"
)

const landingPage = `<!DOCTYPE html>
<html>
<head><title>Build an App</title></head>
<body>
<h1>Build an App</h1>
<p>Describe what you want and we'll build it for you.</p>
<form method="POST" action="/build">
  <p><textarea name="prompt" rows="6" cols="70" placeholder="Describe the app you want to build..."></textarea></p>
  <button type="submit">Build it</button>
</form>
</body>
</html>`

type Server struct {
	store  *Store
	domain string
	port   string
	llm    adkmodel.LLM
}

func NewServer(store *Store, domain, port string, llm adkmodel.LLM) *echo.Echo {
	s := &Server{store: store, domain: domain, port: port, llm: llm}

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	e.Use(middleware.RequestLoggerWithConfig(middleware.RequestLoggerConfig{
		LogMethod:  true,
		LogURI:     true,
		LogStatus:  true,
		LogLatency: true,
		LogHost:    true,
		LogError:   true,
		LogValuesFunc: func(c echo.Context, v middleware.RequestLoggerValues) error {
			level := slog.LevelInfo
			if v.Error != nil || v.Status >= 500 {
				level = slog.LevelError
			}
			slog.Log(c.Request().Context(), level, "request",
				"host", v.Host,
				"method", v.Method,
				"uri", v.URI,
				"status", v.Status,
				"latency", v.Latency.Round(time.Millisecond),
				"error", v.Error,
			)
			return nil
		},
	}))

	e.Use(s.subdomainMiddleware())

	e.GET("/", s.landingHandler)
	e.POST("/build", s.buildHandler)

	return e
}

// subdomainMiddleware intercepts requests to *.domain and proxies them to S3.
// Requests to the main domain (or localhost) fall through to normal routes.
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
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

func (s *Server) landingHandler(c echo.Context) error {
	return c.HTML(http.StatusOK, landingPage)
}

func (s *Server) buildHandler(c echo.Context) error {
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}

	slug := newSlug()
	slog.Info("build.start", "slug", slug)

	if err := runAgent(c.Request().Context(), s.llm, s.store, slug, prompt); err != nil {
		slog.Error("build.failed", "slug", slug, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError, fmt.Sprintf("build failed: %s", err))
	}

	slog.Info("build.done", "slug", slug)
	target := fmt.Sprintf("http://%s.%s:%s", slug, s.domain, s.port)
	return c.Redirect(http.StatusFound, target)
}

func (s *Server) proxyHandler(c echo.Context, slug string) error {
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
		content, err := s.store.Read(ctx, slug, candidate)
		if err != nil {
			return echo.NewHTTPError(http.StatusInternalServerError, err.Error())
		}
		if content != "" {
			return c.HTML(http.StatusOK, content)
		}
	}

	return echo.ErrNotFound
}
