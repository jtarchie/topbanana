package server

// The v2 workspace ("Canvas"): the site itself is the interface. The page
// renders the site full-bleed through the /s/:slug path in edit mode — same
// origin as the admin UI, so the injected selection script can report element
// addresses (see internal/domaddr) back to the floating command bar. The
// classic workspace stays untouched at /workspace/:slug; each links to the
// other while v2 earns its way to default.

import (
	"html/template"
	"net/http"
	"net/url"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
)

type canvasData struct {
	Chrome
	Page     string
	Pages    []string
	EditURL  template.URL
	SlugJSON template.JS
	PageJSON template.JS
	// Building satisfies the shared runfeed_js partial; the canvas drives its
	// own live-run narration, so it always renders false here.
	Building bool
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
	})
}
