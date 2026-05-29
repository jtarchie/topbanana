package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/labstack/echo/v5"
)

// deleteFileHandler removes a single user-owned file from a site. Wired into
// the GitHub-style trash action on the files list and the "Delete file"
// button on the workspace/function editor pages.
//
// Belt-and-suspenders confirmation: the form must include `confirm` equal to
// `path` so an accidental curl (or a stray click that bypasses the JS modal)
// still can't delete the wrong file.
func (s *Server) deleteFileHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	path := c.FormValue("path")
	_, err = classifyUserPath(path)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if c.FormValue("confirm") != path {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match path")
	}

	ctx := c.Request().Context()
	err = s.store.Delete(ctx, slug, path)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete file", err)
	}

	caller := userFromContext(c)
	email := ""
	if caller != nil {
		email = caller.Email
	}
	slog.Info("file.delete", "slug", slug, "path", path, "user", email)

	return c.Redirect(http.StatusSeeOther, fileOpsNextURL(c, slug, "Deleted "+path)) //nolint:wrapcheck
}

// renameFileHandler moves a single user-owned file. The "to" value comes
// either as a bare path (workspace + files list) or as `to_name` for the
// function editor where the prefix and extension are locked. Cross-kind
// renames (HTML → asset, etc.) are rejected so the file stays in an area
// the agent and the rest of the platform understand.
func (s *Server) renameFileHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	from := c.FormValue("from")
	fromKind, err := classifyUserPath(from)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "from: "+err.Error())
	}

	to := c.FormValue("to")
	if to == "" {
		// Function editor uses a name-only field so the user can't type a
		// path that escapes functions/<...>.js.
		if name := c.FormValue("to_name"); name != "" {
			to = "functions/" + name + ".js"
		}
	}
	toKind, err := classifyUserPath(to)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "to: "+err.Error())
	}
	if fromKind != toKind {
		return echo.NewHTTPError(http.StatusBadRequest,
			"cannot rename across file kinds ("+fromKind.String()+" → "+toKind.String()+")")
	}

	ctx := c.Request().Context()
	if from != to {
		existing, readErr := s.store.Read(ctx, slug, to)
		if readErr != nil {
			return httpErr(http.StatusInternalServerError, "check destination", readErr)
		}
		if existing != nil && existing.Content != "" {
			return echo.NewHTTPError(http.StatusBadRequest, "destination "+to+" already exists")
		}
	}

	err = s.store.Rename(ctx, slug, from, to)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "rename file", err)
	}

	caller := userFromContext(c)
	email := ""
	if caller != nil {
		email = caller.Email
	}
	slog.Info("file.rename", "slug", slug, "from", from, "to", to, "user", email)

	dest := editorURLFor(slug, to, toKind)
	flash := "Renamed " + from + " to " + to
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	return c.Redirect(http.StatusSeeOther, dest+sep+"flash="+urlEscape(flash)) //nolint:wrapcheck
}

// editorURLFor returns the natural landing page for a file after a rename:
// the workspace page editor for HTML, the function editor for handlers, and
// the files list for assets (which don't have a per-file editor).
func editorURLFor(slug, path string, kind fileKind) string {
	switch kind {
	case kindHTML:
		return "/workspace/" + slug + "?page=" + url.QueryEscape(path)
	case kindFunction:
		name := strings.TrimSuffix(strings.TrimPrefix(path, "functions/"), ".js")
		return "/edit/" + slug + "/function/" + name
	case kindAsset:
		return "/files/" + slug
	default:
		return "/files/" + slug
	}
}

// fileOpsNextURL honors a `next` form field when it points at one of our own
// admin routes for the same slug, otherwise lands on the files list. The
// allowlist keeps an open redirect off the table — POST forms can't be used
// to bounce a logged-in user off-site via this handler.
func fileOpsNextURL(c *echo.Context, slug, flash string) string {
	fallback := "/files/" + slug + "?flash=" + urlEscape(flash)
	next := c.FormValue("next")
	if next == "" {
		return fallback
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return fallback
	}
	allowed := []string{
		"/files/" + slug,
		"/workspace/" + slug,
		"/manage/" + slug,
	}
	for _, prefix := range allowed {
		if next == prefix || strings.HasPrefix(next, prefix+"?") || strings.HasPrefix(next, prefix+"/") {
			sep := "?"
			if strings.Contains(next, "?") {
				sep = "&"
			}
			return next + sep + "flash=" + urlEscape(flash)
		}
	}
	return fallback
}
