package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/templates"
)

// manageData is the single struct backing the consolidated /manage/:slug page.
// It carries everything that was previously split across the settings page and
// the form-submissions page so the user sees one config surface, not three.
type manageData struct {
	Slug             string
	SiteName         string
	SiteURL          string
	Active           string
	Title            string
	Domains          string
	FunctionsEnabled bool
	FunctionsByTmpl  bool
	PublicAPIEnabled bool
	Columns          []string
	Rows             []dataRow
	CSVURL           string
	Flash            string
}

func (s *Server) manageHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	base := templates.Get(meta.Template)
	byTmpl := base != nil && base.EnablesFunctions
	tmpl := build.EffectiveTemplate(meta)

	cols, rows, err := s.collectSubmissions(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "load submissions", err)
	}

	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}

	return s.render(c, "manage", manageData{
		Slug:             slug,
		SiteName:         siteName,
		SiteURL:          s.siteURL(c, slug, "/"),
		Active:           "manage",
		Title:            meta.Title,
		Domains:          strings.Join(meta.Domains, "\n"),
		FunctionsEnabled: tmpl != nil && tmpl.EnablesFunctions,
		FunctionsByTmpl:  byTmpl,
		PublicAPIEnabled: meta.EnablesPublicAPI,
		Columns:          cols,
		Rows:             rows,
		CSVURL:           "/data/" + slug + "?format=csv",
		Flash:            c.QueryParam("flash"),
	})
}
