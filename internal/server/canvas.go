package server

// The v2 workspace ("Canvas"): the site itself is the interface. The page
// renders the site full-bleed through the /s/:slug path in edit mode — same
// origin as the admin UI, so the injected selection script can report element
// addresses (see internal/domaddr) back to the floating command bar. The
// classic workspace stays untouched at /workspace/:slug; each links to the
// other while v2 earns its way to default.

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/domaddr"
)

type canvasData struct {
	Chrome
	Page     string
	Pages    []string
	EditURL  template.URL
	SlugJSON template.JS
	PageJSON template.JS
	// Building is true when a run is already in flight for this slug (started
	// from the classic workspace, MCP, or another tab). The canvas reattaches
	// to it on load — without this the page renders an idle composer that 409s
	// on submit, and the in-flight run's completion never updates the page.
	Building bool
}

// resolveScopeBlock turns the canvas's element selection (page + address)
// into the agent-facing context block, resolved entirely from the stored
// source via domaddr — the client contributes only two small numbers/names,
// both validated here. Empty scope_el means an unscoped edit. A stale
// address (the page changed since it was served) is a user-facing error that
// tells them to reselect, not a silent mis-scope.
func (s *Server) resolveScopeBlock(ctx context.Context, slug, scopePage, scopeEl string) (string, error) {
	scopeEl = strings.TrimSpace(scopeEl)
	if scopeEl == "" {
		return "", nil
	}
	n, err := strconv.Atoi(scopeEl)
	if err != nil || n < 0 {
		return "", errors.New("the selection is invalid — click the element again")
	}
	err = validatePage(scopePage)
	if err != nil || scopePage == "" {
		return "", errors.New("the selection is missing its page — click the element again")
	}
	obj, err := s.store.Read(ctx, slug, scopePage)
	if err != nil || obj == nil || obj.Content == "" {
		return "", errors.New("the selected page no longer exists — reload and try again")
	}
	outer, err := domaddr.OuterHTML([]byte(obj.Content), n)
	if err != nil {
		return "", errors.New("the page changed since you selected — click the element again")
	}
	markup := string(outer)
	if len(markup) > maxScopeBytes {
		markup = strings.ToValidUTF8(markup[:maxScopeBytes], "") + "…"
	}
	return fmt.Sprintf(
		"The user selected one specific element on %s and the change applies to THAT element only — not to other elements with similar content. It is element #%d of the page in document order. Its current source markup:\n\n```html\n%s\n```",
		scopePage, n, markup), nil
}

func (s *sitesController) canvasHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	page := c.QueryParam("page")
	if page == "" {
		page = "index.html"
	}
	err = validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	all, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list pages", err)
	}
	pages, _ := build.SplitFilesByKind(all)
	meta := s.build.ReadMeta(ctx, slug)
	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}

	editURL := "/s/" + url.PathEscape(slug) + "/" + page + "?tb_edit=1"

	return s.render(c, "canvas", canvasData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "workspace",
		},
		Page:     page,
		Pages:    pages,
		EditURL:  template.URL(editURL), //nolint:gosec // built from a validated slug and page above.
		SlugJSON: toJSONLiteral(slug),
		PageJSON: toJSONLiteral(page),
		Building: s.buildInFlight(slug),
	})
}
