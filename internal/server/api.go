package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/sandbox"
	"github.com/jtarchie/buildabear/internal/templates"
)

// maxAPIBodyBytes caps incoming /api/* request bodies. The sandbox enforces
// its own response cap; this is the matching ingress side. Conservative — most
// form posts are well under a kilobyte.
const maxAPIBodyBytes = 256 * 1024

// apiHandler dispatches GET/POST/etc. to functions/{name}.js inside the slug's
// store. Per-template opt-in: returns 404 for slugs whose template did not
// enable functions, so brochure sites stay byte-for-byte unchanged.
func (s *Server) apiHandler(c *echo.Context, slug, name string) error {
	if s.sandbox == nil {
		return echo.ErrNotFound
	}

	ctx := c.Request().Context()

	meta := s.build.ReadMeta(ctx, slug)
	if meta.Template == "" {
		// No metadata sidecar — sites created before templates existed don't
		// have functions. Treat as not-found.
		return echo.ErrNotFound
	}
	if !s.templateEnablesFunctions(meta.Template) {
		return echo.ErrNotFound
	}

	if !verifyBasicAuth(meta, c.Request()) {
		c.Response().Header().Set("WWW-Authenticate", `Basic realm="`+slug+`"`)
		return c.NoContent(http.StatusUnauthorized) //nolint:wrapcheck
	}

	err := validateFunctionPathName(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	src, err := s.loadFunctionSource(ctx, slug, name)
	if err != nil {
		if errors.Is(err, errFunctionNotFound) {
			return echo.ErrNotFound
		}
		return httpErr(http.StatusInternalServerError, "load function", err)
	}

	req, err := buildSandboxRequest(c.Request(), name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	logFn := func(level, line string) {
		slog.Info("function.log", "slug", slug, "fn", name, "level", level, "line", line)
		s.events.Emit(slug, events.Event{
			Type:    events.TypeFunction,
			Tool:    name,
			Phase:   events.PhaseLog,
			Message: level + ": " + line,
		})
	}

	s.events.Emit(slug, events.Event{Type: events.TypeFunction, Tool: name, Phase: events.PhaseInvoke})
	// snap=nil for Phase 1: handlers can return responses but have no kv state.
	// Commit 5 (Phase 2 b) wires state.Store in here.
	resp, err := s.sandbox.Invoke(ctx, slug, name, src, req, nil, logFn)
	if err != nil {
		return translateSandboxError(err, slug, name)
	}
	return writeSandboxResponse(c, resp)
}

var errFunctionNotFound = errors.New("function not found")

// loadFunctionSource fetches the JS source from S3. Empty content (which the
// store returns for missing objects with no error) becomes errFunctionNotFound
// so the caller can map it to a 404.
func (s *Server) loadFunctionSource(ctx context.Context, slug, name string) (string, error) {
	path := "functions/" + name + ".js"
	obj, err := s.store.Read(ctx, slug, path)
	if err != nil {
		return "", fmt.Errorf("read function: %w", err)
	}
	if obj.Content == "" {
		return "", errFunctionNotFound
	}
	return obj.Content, nil
}

func (s *Server) templateEnablesFunctions(id string) bool {
	tmpl := templates.Get(id)
	return tmpl != nil && tmpl.EnablesFunctions
}

// validateFunctionPathName matches the agent-side validateFunctionName so the
// router can only resolve to handlers the agent could have written.
func validateFunctionPathName(name string) error {
	if name == "" || len(name) > 40 {
		return errors.New("invalid function name")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return errors.New("invalid function name")
		}
	}
	return nil
}

// buildSandboxRequest copies headers/query/body off the *http.Request into a
// sandbox-friendly form. Headers are lowercased. Body is capped at
// maxAPIBodyBytes; over-cap requests get a 413 via the returned error.
func buildSandboxRequest(r *http.Request, name string) (sandbox.Request, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBodyBytes+1))
	if err != nil {
		return sandbox.Request{}, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxAPIBodyBytes {
		return sandbox.Request{}, fmt.Errorf("body exceeds %d bytes", maxAPIBodyBytes)
	}

	q := map[string]string{}
	for k, vs := range r.URL.Query() {
		if len(vs) > 0 {
			q[k] = vs[0]
		}
	}
	h := map[string]string{}
	for k, vs := range r.Header {
		if len(vs) > 0 {
			h[strings.ToLower(k)] = vs[0]
		}
	}

	req := sandbox.Request{
		Method:  r.Method,
		Path:    "/api/" + name,
		Query:   q,
		Headers: h,
		Body:    string(body),
	}

	ct := strings.ToLower(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	switch ct {
	case "application/x-www-form-urlencoded":
		// Parse the captured body directly — r.PostForm would re-read r.Body.
		vals, perr := url.ParseQuery(string(body))
		if perr == nil {
			form := map[string]string{}
			for k, vs := range vals {
				if len(vs) > 0 {
					form[k] = vs[0]
				}
			}
			req.Form = form
		}
	case "application/json":
		// Surface as `request.json` only when the body is valid JSON; the raw
		// string is always available via request.body.
		var parsed any
		err := json.Unmarshal(body, &parsed)
		if err == nil {
			req.JSON = parsed
		}
	}

	return req, nil
}

// translateSandboxError maps sandbox-level errors to HTTP responses. Compile
// errors and missing-handler errors are user-visible (the agent put something
// invalid in S3) so we log and 500. Rate limits and timeouts get their own
// status codes so the caller can tell them apart.
func translateSandboxError(err error, slug, name string) error {
	switch {
	case errors.Is(err, sandbox.ErrRateLimit):
		slog.Warn("api.rate_limited", "slug", slug, "fn", name)
		return echo.NewHTTPError(http.StatusTooManyRequests, "rate limit exceeded")
	case errors.Is(err, sandbox.ErrTimeout):
		slog.Warn("api.timeout", "slug", slug, "fn", name)
		return echo.NewHTTPError(http.StatusGatewayTimeout, "function timed out")
	case errors.Is(err, sandbox.ErrCompile):
		slog.Error("api.compile_failed", "slug", slug, "fn", name, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "function failed to compile")
	case errors.Is(err, sandbox.ErrNoHandler):
		slog.Error("api.no_handler", "slug", slug, "fn", name)
		return echo.NewHTTPError(http.StatusInternalServerError, "function has no handler")
	case errors.Is(err, sandbox.ErrResponseTooLarge):
		slog.Warn("api.response_too_large", "slug", slug, "fn", name)
		return echo.NewHTTPError(http.StatusInternalServerError, "function response too large")
	default:
		slog.Error("api.invoke_failed", "slug", slug, "fn", name, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "function failed")
	}
}

func writeSandboxResponse(c *echo.Context, resp sandbox.Response) error {
	for k, v := range resp.Headers {
		c.Response().Header().Set(k, v)
	}
	status := resp.Status
	if status == 0 {
		status = http.StatusOK
	}
	ct := resp.ContentType
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}
	if status == http.StatusNoContent || len(resp.Body) == 0 {
		return c.NoContent(status) //nolint:wrapcheck
	}
	return c.Blob(status, ct, resp.Body) //nolint:wrapcheck
}
