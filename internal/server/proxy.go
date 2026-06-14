package server

import (
	"bytes"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/store"
)

// This file owns the static-serve path: the per-slug proxy that reads stored
// objects out of S3 and serves them, plus its cache-header policy, content-type
// resolution, and the edit-toolbar/listener injection spliced into served HTML.
// Extracted from server.go so the serve path is navigable on its own.

// reservedProxyPrefixes are bucket paths the static proxy must never serve.
// Slugs themselves can't start with "_" (validateSlug), so these only apply
// to paths *within* a real slug — e.g. blocking GET /_state/data.json from
// leaking persisted form data on a site at slug.example.com.
var reservedProxyPrefixes = []string{store.StateDir, ".topbanana/", ".bloomhollow/", ".buildabear/"}

// reservedProxyPaths are exact bucket paths the static proxy must never
// serve. `.topbanana.json` is the per-site metadata sidecar; `.bloomhollow.json`
// and `.buildabear.json` are pre-rebrand names kept reserved so legacy sites
// can't leak metadata if the new file is missing.
var reservedProxyPaths = map[string]bool{
	".topbanana.json":   true,
	".bloomhollow.json": true,
	".buildabear.json":  true,
}

func (s *Server) proxyHandler(c *echo.Context, slug string) error {
	ctx := c.Request().Context()

	reqPath := strings.TrimPrefix(c.Request().URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}

	// Reject traversal *before* the reserved-prefix check — otherwise a path
	// like `assets/../_state/data.json` slips past HasPrefix("_state/").
	if isTraversal(reqPath) || reservedProxyPaths[reqPath] {
		return notFound()
	}
	for _, pfx := range reservedProxyPrefixes {
		if strings.HasPrefix(reqPath, pfx) {
			return notFound()
		}
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
			body := s.injectEditToolbar(c, obj.Content, slug, candidate)
			minified, mErr := minifyHTMLBody(s.htmlMinifier, body)
			if mErr != nil {
				slog.Warn("serve.minify_failed", "slug", slug, "path", candidate, "err", mErr)
			}
			return c.HTML(http.StatusOK, minified) //nolint:wrapcheck
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

// injectEditToolbar inserts the theme-preview listener and (for owners) the
// edit toolbar before </body>. Skipped entirely on custom-domain responses
// so the CDN never caches admin bytes. On the platform domain the listener
// always ships — the workspace iframe needs it to drive live theme preview.
// It loads without the admin session cookie because the cookie isn't scoped
// to subdomains, and is a no-op without a postMessage opener, so direct
// visitors see no behavior change. The visible toolbar (edit links) stays
// gated on canEdit, since that does leak ownership. Returns the content
// unchanged when there's no </body> to splice into.
func (s *Server) injectEditToolbar(c *echo.Context, htmlContent, slug, page string) string {
	if c.Get("custom_domain") == true {
		return htmlContent
	}
	if !strings.Contains(htmlContent, "</body>") {
		return htmlContent
	}

	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, "theme_preview_listener", nil)
	if err != nil {
		slog.Warn("theme_preview_listener.render_failed", "slug", slug, "err", err)
		return htmlContent
	}

	if s.canEdit(c, slug) {
		q := url.Values{"page": []string{page}}.Encode()
		editURL := s.adminURL(c, "/edit/"+slug) + "?" + q
		visualURL := s.adminURL(c, "/edit/"+slug+"/visual") + "?" + q

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
	} else {
		// canEdit-less branch still needs a </body> in the spliced payload
		// so the document closes properly after the listener.
		buf.WriteString("</body>")
	}

	return strings.Replace(htmlContent, "</body>", buf.String(), 1)
}
