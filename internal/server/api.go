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
	"sort"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/bloomhollow/internal/build"
	"github.com/jtarchie/bloomhollow/internal/events"
	"github.com/jtarchie/bloomhollow/internal/sandbox"
	"github.com/jtarchie/bloomhollow/internal/state"
)

// maxCASRetries caps the number of times we'll re-run a handler after an
// ErrConflict from state.Store.Save. Three is enough to ride through bursts
// of two or three concurrent writers; beyond that we surface 503 so callers
// back off.
const maxCASRetries = 3

// maxAPIBodyBytes caps incoming /api/* request bodies. The sandbox enforces
// its own response cap; this is the matching ingress side. Conservative — most
// form posts are well under a kilobyte.
const maxAPIBodyBytes = 256 * 1024

// maxAPIFieldBytes caps each individual form field (or top-level JSON string)
// arriving at /api/*. Catches the case where the overall body budget is
// blown on a single oversized field — a handler that forgets to validate
// still can't ingest megabytes into one column.
const maxAPIFieldBytes = 4 * 1024

// errAPIPayloadTooLarge signals an oversize-input failure that should map to
// HTTP 413 rather than the generic 400 buildSandboxRequest otherwise returns.
var errAPIPayloadTooLarge = errors.New("payload too large")

// setAPICacheHeaders marks an /api/* response as uncacheable. Necessary on
// custom domains (CDN safety) and harmless on subdomain previews.
func setAPICacheHeaders(c *echo.Context) {
	h := c.Response().Header()
	h.Set("Cache-Control", "no-store, private")
	h.Set("Pragma", "no-cache")
	h.Set("Vary", "*")
}

// apiHandler dispatches GET/POST/etc. to functions/{name}.js inside the slug's
// store. Per-template opt-in: returns 404 for slugs whose template did not
// enable functions, so brochure sites stay byte-for-byte unchanged.
func (s *Server) apiHandler(c *echo.Context, slug, name string) error {
	// /api/* responses are dynamic per-call (CAS reads/writes against the KV
	// store). Set no-store unconditionally — even on 404s — so a CDN never
	// caches a stale answer, including the "functions disabled" case that
	// could later be flipped on.
	setAPICacheHeaders(c)

	if s.sandbox == nil {
		return notFound()
	}

	ctx := c.Request().Context()

	meta := s.build.ReadMeta(ctx, slug)
	if meta.Template == "" {
		// No metadata sidecar — sites created before templates existed don't
		// have functions. Treat as not-found.
		return notFound()
	}
	tmpl := build.EffectiveTemplate(meta)
	if tmpl == nil || !tmpl.EnablesFunctions {
		return notFound()
	}

	err := checkAPIOrigin(c.Request(), meta)
	if err != nil {
		return err
	}

	err = validateFunctionPathName(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	src, err := s.loadFunctionSource(ctx, slug, name)
	if err != nil {
		if errors.Is(err, errFunctionNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load function", err)
	}

	req, err := buildSandboxRequest(c.Request(), name)
	if err != nil {
		if errors.Is(err, errAPIPayloadTooLarge) {
			return echo.NewHTTPError(http.StatusRequestEntityTooLarge, err.Error())
		}
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

	resp, err := s.invokeWithCAS(ctx, slug, name, src, req, logFn)
	if err != nil {
		return translateSandboxError(err, slug, name)
	}
	return writeSandboxResponse(c, resp)
}

// invokeWithCAS runs the handler with an optional Snapshot, then persists the
// snapshot if the handler marked it dirty. On ErrConflict (a concurrent writer
// won the race), the whole handler is re-run with the freshly-loaded snapshot
// up to maxCASRetries times before surfacing 503 via ErrConflict.
func (s *Server) invokeWithCAS(ctx context.Context, slug, name, src string, req sandbox.Request, logFn sandbox.LogFn) (sandbox.Response, error) {
	if s.state == nil {
		// No state backend wired: invoke stateless. Brochure paths and unit
		// tests both rely on this branch.
		return s.sandbox.Invoke(ctx, slug, name, src, req, nil, logFn) //nolint:wrapcheck
	}

	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		snap, err := s.state.Load(ctx, slug)
		if err != nil {
			return sandbox.Response{}, fmt.Errorf("state load: %w", err)
		}
		resp, err := s.sandbox.Invoke(ctx, slug, name, src, req, snap, logFn)
		if err != nil {
			return sandbox.Response{}, err //nolint:wrapcheck
		}
		if !snap.Dirty {
			return resp, nil
		}
		err = s.state.Save(ctx, slug, snap)
		if err == nil {
			return resp, nil
		}
		if errors.Is(err, state.ErrConflict) {
			slog.Info("api.cas_retry", "slug", slug, "fn", name, "attempt", attempt+1)
			continue
		}
		return sandbox.Response{}, fmt.Errorf("state save: %w", err)
	}
	// Exhausted retries — return the conflict sentinel so translateSandboxError
	// can map it to 503.
	return sandbox.Response{}, state.ErrConflict
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

// checkAPIOrigin rejects state-changing /api/* requests whose Origin/Referer
// header doesn't match the request Host. Mirrors the Rails-style supplemental
// CSRF check — hosted sites have no per-user session so token-based CSRF
// doesn't apply, but an origin check still cuts drive-by cross-origin
// submission spam.
//
// Idempotent methods (GET, HEAD, OPTIONS) bypass the check so pages can fetch
// their own JSON endpoints normally. Sites that genuinely want a public API
// (webhooks, public JSON endpoints) opt out via SiteMeta.EnablesPublicAPI on
// the settings page.
//
// Fails closed when both Origin and Referer are missing: modern browsers
// always send at least one on POSTs, so the absence usually means a scripted
// client. EnablesPublicAPI is the escape hatch for those.
func checkAPIOrigin(r *http.Request, meta build.SiteMeta) error {
	if meta.EnablesPublicAPI {
		return nil
	}
	if isAPISafeMethod(r.Method) {
		return nil
	}
	origin := r.Header.Get("Origin")
	referer := r.Header.Get("Referer")
	host := r.Host
	if apiOriginMatches(origin, host) || apiOriginMatches(referer, host) {
		return nil
	}
	return echo.NewHTTPError(http.StatusForbidden,
		"cross-origin /api/* requests are blocked by default; enable public API in settings to allow")
}

func isAPISafeMethod(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// apiOriginMatches reports whether the Origin or Referer header value names
// the same host (port-insensitive) as the request Host. Empty header always
// fails — the caller treats absent Origin AND absent Referer as a rejection.
func apiOriginMatches(headerVal, host string) bool {
	if headerVal == "" || host == "" {
		return false
	}
	u, err := url.Parse(headerVal)
	if err != nil || u.Host == "" {
		return false
	}
	return stripPort(u.Host) == stripPort(host)
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
// maxAPIBodyBytes; individual form fields and top-level JSON string values
// are capped at maxAPIFieldBytes. Over-cap requests return errAPIPayloadTooLarge.
func buildSandboxRequest(r *http.Request, name string) (sandbox.Request, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxAPIBodyBytes+1))
	if err != nil {
		return sandbox.Request{}, fmt.Errorf("read body: %w", err)
	}
	if len(body) > maxAPIBodyBytes {
		return sandbox.Request{}, fmt.Errorf("%w: body exceeds %d bytes", errAPIPayloadTooLarge, maxAPIBodyBytes)
	}

	req := sandbox.Request{
		Method:  r.Method,
		Path:    "/api/" + name,
		Query:   firstValues(r.URL.Query()),
		Headers: lowercasedFirstValues(r.Header),
		Body:    string(body),
	}

	ct := strings.ToLower(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0])
	switch ct {
	case "application/x-www-form-urlencoded":
		form, ferr := parseFormBody(body)
		if ferr != nil {
			return sandbox.Request{}, ferr
		}
		req.Form = form
	case "application/json":
		parsed, jerr := parseJSONBody(body)
		if jerr != nil {
			return sandbox.Request{}, jerr
		}
		req.JSON = parsed
	}

	return req, nil
}

// firstValues collapses a url.Values-style map (each key has 0+ values) to a
// single-value map by picking the first. Empty slots drop.
func firstValues(in map[string][]string) map[string]string {
	out := map[string]string{}
	for k, vs := range in {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}

// lowercasedFirstValues is like firstValues but lowercases the keys — used for
// HTTP headers so handlers don't have to case-match.
func lowercasedFirstValues(in map[string][]string) map[string]string {
	out := map[string]string{}
	for k, vs := range in {
		if len(vs) > 0 {
			out[strings.ToLower(k)] = vs[0]
		}
	}
	return out
}

// parseFormBody pulls form-encoded values out of body. Returns an empty map
// on parse failure (matches the prior behavior) and a non-nil error only when
// a single field exceeds maxAPIFieldBytes.
func parseFormBody(body []byte) (map[string]string, error) {
	form := map[string]string{}
	vals, perr := url.ParseQuery(string(body))
	if perr != nil {
		return form, nil
	}
	for k, vs := range vals {
		if len(vs) == 0 {
			continue
		}
		v := vs[0]
		if len(v) > maxAPIFieldBytes {
			return nil, fmt.Errorf("%w: form field %q exceeds %d bytes", errAPIPayloadTooLarge, k, maxAPIFieldBytes)
		}
		form[k] = v
	}
	return form, nil
}

// parseJSONBody surfaces parsed JSON via request.json when the body is valid
// JSON. Top-level string values are size-checked the same way as form fields
// so a handler that reads request.json.bio can't be handed a 200 KiB blob.
// Invalid JSON degrades to a nil parsed value (request.body still carries the
// raw bytes for handlers that want it).
func parseJSONBody(body []byte) (any, error) {
	var parsed any
	err := json.Unmarshal(body, &parsed)
	if err != nil {
		return parsed, nil
	}
	if obj, ok := parsed.(map[string]any); ok {
		for k, v := range obj {
			s, isStr := v.(string)
			if !isStr {
				continue
			}
			if len(s) > maxAPIFieldBytes {
				return nil, fmt.Errorf("%w: json field %q exceeds %d bytes", errAPIPayloadTooLarge, k, maxAPIFieldBytes)
			}
		}
	}
	return parsed, nil
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
	case errors.Is(err, state.ErrConflict):
		slog.Warn("api.cas_exhausted", "slug", slug, "fn", name)
		return echo.NewHTTPError(http.StatusServiceUnavailable, "state contention — retry shortly")
	default:
		slog.Error("api.invoke_failed", "slug", slug, "fn", name, "err", err)
		return echo.NewHTTPError(http.StatusInternalServerError, "function failed")
	}
}

// functionEditHandler renders the per-function source-view + test page. The
// test endpoint POSTs JSON to /test/:slug/api/:name; live log streaming reuses
// the existing /events/:slug SSE feed.
func (s *Server) functionEditHandler(c *echo.Context) error {
	slug := c.Param("slug")
	name := c.Param("name")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	err = validateFunctionPathName(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	ctx := c.Request().Context()
	obj, err := s.store.Read(ctx, slug, "functions/"+name+".js")
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read function", err)
	}
	if obj.Content == "" {
		return notFound()
	}
	return s.render(c, "function_edit", functionEditData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: s.siteNameOrSlug(ctx, slug),
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "workspace",
		},
		Name:   name,
		APIURL: s.siteURL(c, slug, "/api/"+name),
		Source: obj.Content,
	})
}

// functionEditData backs templates/function_edit.html. Was a
// map[string]any until the chrome refactor; the typed struct lets the
// shared brand partial pick up IsSuperAdmin via embedded promotion.
type functionEditData struct {
	Chrome
	Name   string
	APIURL string
	Source string
}

// functionTestRequest is the JSON body the editor sends to /test/:slug/api/:name.
type functionTestRequest struct {
	Method      string `json:"method"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
}

// functionTestResponse is what the editor renders. Headers are passed through
// verbatim so the editor can show what the function set (Location, etc.).
type functionTestResponse struct {
	Status      int               `json:"status"`
	ContentType string            `json:"content_type"`
	Headers     map[string]string `json:"headers,omitempty"`
	Body        string            `json:"body"`
}

func (s *Server) functionTestHandler(c *echo.Context) error {
	slug := c.Param("slug")
	name := c.Param("name")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	err = validateFunctionPathName(name)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	var in functionTestRequest
	err = c.Bind(&in)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid test request: "+err.Error())
	}
	if in.Method == "" {
		in.Method = http.MethodGet
	}

	ctx := c.Request().Context()
	src, err := s.loadFunctionSource(ctx, slug, name)
	if err != nil {
		if errors.Is(err, errFunctionNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load function", err)
	}

	req := sandbox.Request{
		Method:  in.Method,
		Path:    "/api/" + name,
		Query:   map[string]string{},
		Headers: map[string]string{"content-type": in.ContentType},
		Body:    in.Body,
	}
	parseTestRequestBody(&req, in.ContentType, in.Body)

	logFn := func(level, line string) {
		slog.Info("function.test_log", "slug", slug, "fn", name, "level", level, "line", line)
		s.events.Emit(slug, events.Event{
			Type: events.TypeFunction, Tool: name, Phase: events.PhaseLog,
			Message: "(test) " + level + ": " + line,
		})
	}

	s.events.Emit(slug, events.Event{
		Type: events.TypeFunction, Tool: name, Phase: events.PhaseInvoke, Message: "(test)",
	})
	resp, err := s.invokeWithCAS(ctx, slug, name, src, req, logFn)
	if err != nil {
		return translateSandboxError(err, slug, name)
	}
	return c.JSON(http.StatusOK, functionTestResponse{ //nolint:wrapcheck
		Status:      resp.Status,
		ContentType: resp.ContentType,
		Headers:     resp.Headers,
		Body:        string(resp.Body),
	})
}

// parseTestRequestBody pre-parses the test body the same way real requests
// would arrive, so the function sees `request.form` / `request.json` when the
// content-type matches.
func parseTestRequestBody(req *sandbox.Request, ct, body string) {
	switch strings.ToLower(strings.SplitN(ct, ";", 2)[0]) {
	case "application/x-www-form-urlencoded":
		vals, err := url.ParseQuery(body)
		if err != nil {
			return
		}
		form := map[string]string{}
		for k, vs := range vals {
			if len(vs) > 0 {
				form[k] = vs[0]
			}
		}
		req.Form = form
	case "application/json":
		var parsed any
		err := json.Unmarshal([]byte(body), &parsed)
		if err == nil {
			req.JSON = parsed
		}
	}
}

// collectFunctionNames extracts the bare handler names from a slug's file
// listing. `functions/submit.js` → `submit`. Returned in stable order so the
// editor render is deterministic.
func collectFunctionNames(files []string) []string {
	names := make([]string, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f, "functions/") || !strings.HasSuffix(f, ".js") {
			continue
		}
		bare := strings.TrimSuffix(strings.TrimPrefix(f, "functions/"), ".js")
		if bare != "" {
			names = append(names, bare)
		}
	}
	sort.Strings(names)
	return names
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
