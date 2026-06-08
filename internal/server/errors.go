package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"
)

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

// errorData backs templates/error.html. The page is intentionally chrome-
// less — it works on subdomains where there's no logged-in user as well
// as on admin pages where we'd rather not lean on injectChrome to re-fetch
// session state during an error response. HomeURL is an absolute URL
// pointing at the admin host, so the "Take me home" CTA gets the user out
// of a broken subdomain (where bare "/" is the user's own site root) and
// back to Top Banana proper.
type errorData struct {
	Status  int
	Title   string
	Tagline string
	HomeURL string
	// Detail carries the specific, actionable reason for a client (4xx) error
	// — e.g. "attachment ... must end in .md, .html" — so the user can fix it.
	// Left empty for 5xx so we never leak wrapped internal error text.
	Detail string
}

// httpErrorHandler replaces Echo's default JSON serializer. Browser
// navigations (Accept: text/html) get the monkey page; everything else —
// fetch() with default Accept, the SSE poller, /api/* function callers,
// curl — keeps the original {"message": "..."} JSON so no client breaks.
func (s *Server) httpErrorHandler(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	code := http.StatusInternalServerError
	msg := http.StatusText(code)
	var he *echo.HTTPError
	if errors.As(err, &he) {
		code = he.Code
		if he.Message != "" {
			msg = he.Message
		}
	}

	if !wantsHTML(c) {
		_ = c.JSON(code, map[string]string{"message": msg})
		return
	}

	title, tagline := errorCopyForStatus(code)
	// Surface the specific reason only for client errors, and only when it
	// carries information beyond the generic status text. 5xx messages often
	// wrap internal error chains (via httpErr), so they stay hidden.
	detail := ""
	if code >= 400 && code < 500 && msg != http.StatusText(code) {
		detail = msg
	}
	var buf bytes.Buffer
	rErr := s.tpl.ExecuteTemplate(&buf, "error", errorData{
		Status:  code,
		Title:   title,
		Tagline: tagline,
		HomeURL: s.adminURL(c, "/"),
		Detail:  detail,
	})
	if rErr != nil {
		_ = c.String(code, msg)
		return
	}
	_ = c.HTML(code, buf.String())
}

// errorCopyForStatus picks a title + tagline pair that matches the status
// family. 4xx is the user found a stale or wrong URL; 5xx is the platform
// stumbled; 401/403 are authorization-class. Keeps the banana wordplay
// for 404 (the "missing banana" line is the brand joke working as
// intended) and lands on sober copy for 5xx where wit reads as glib.
func errorCopyForStatus(code int) (title, tagline string) {
	switch {
	case code == http.StatusUnauthorized:
		return "Sign in to continue.", "You'll need to sign in before opening that page."
	case code == http.StatusForbidden:
		return "You don't have access to that.", "If this is your site, check that you're signed in with the right account."
	case code == http.StatusNotFound:
		return "This bunch is missing a banana.", "We couldn't find that page. The link may be stale, or the site may have been renamed."
	case code >= 500:
		return "We slipped on a peel.", "Something on our side broke. Try again in a moment, or head home and start fresh."
	case code >= 400:
		return "We couldn't process that.", "The request didn't quite land. Try going back, or head home."
	default:
		return "Something went sideways.", "Try going back, or head home and start fresh."
	}
}

// wantsHTML returns true for plain browser navigations. fetch(), curl, and
// SSE clients leave Accept unset or send `*/*`, which we treat as JSON to
// preserve the existing API contract.
func wantsHTML(c *echo.Context) bool {
	return strings.Contains(c.Request().Header.Get("Accept"), "text/html")
}
