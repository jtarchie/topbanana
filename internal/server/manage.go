package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/templates"
)

// manageSubmissionLimit caps how many submission rows we render inline on
// /manage/:slug. Beyond this, the page shows a "+ N more" note and the CSV
// download is the path to the full set. Pagination would add clicks and
// state to a screen most users skim; CSV in a spreadsheet is the better
// tool for bulk analysis anyway.
const manageSubmissionLimit = 25

// manageData is the single struct backing the consolidated /manage/:slug page.
// It carries everything that was previously split across the settings page and
// the form-submissions page so the user sees one config surface, not three.
type manageData struct {
	Slug             string
	SiteName         string
	SiteURL          string
	Active           string
	IsSuperAdmin     bool // populated by s.render via injectChrome.
	Title            string
	Domains          string
	FunctionsEnabled bool
	FunctionsByTmpl  bool
	PublicAPIEnabled bool
	Columns          []string
	Rows             []dataRow // capped at manageSubmissionLimit
	// TotalRows is the unsliced count so the template can render
	// "+ N more, download CSV for all".
	TotalRows int
	// MoreCount is TotalRows - len(Rows), exposed pre-computed because
	// html/template has no arithmetic helpers.
	MoreCount int
	CSVURL    string
	JSONURL   string
	Flash     string
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
	total := len(rows)
	if total > manageSubmissionLimit {
		rows = rows[:manageSubmissionLimit]
	}
	more := total - len(rows)

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
		TotalRows:        total,
		MoreCount:        more,
		CSVURL:           "/data/" + slug + "?format=csv",
		JSONURL:          "/data/" + slug + "?format=json",
		Flash:            c.QueryParam("flash"),
	})
}
