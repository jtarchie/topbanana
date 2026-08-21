package server

import (
	"context"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
)

// workspaceData backs the unified design surface at /workspace/:slug. It
// folds together what used to be three separate pages — Edit, Appearance,
// and Version history — by carrying the same per-page edit data the old
// editHandler exposed plus the theme picker and snapshot list as inline
// panels.
type workspaceData struct {
	Chrome
	PageURL string
	Page    string
	Pages   []string
	// Stylesheets are the sheets the site authored for itself. Listed
	// separately from Pages because SplitFilesByKind buckets neither: a
	// root-level site.css is not a page and not an upload. Without this the
	// agent edits a file on every visual request, and lint names it in errors,
	// that the owner cannot see anywhere in the workspace.
	Stylesheets []string
	Assets      []editAsset
	Functions   []string
	Flash       string

	// Building flag flips the status strip on and hides the preview behind a
	// placeholder. Set from ?building=1 (right after POST /build or POST
	// /edit/:slug) or when the events tracker says a run is in flight (handles
	// mid-build page refreshes).
	Building bool

	// Theme picker panel
	CurrentTheme     string
	SlugJSON         template.JS
	CurrentThemeJSON template.JS
	ThemesJSON       template.JS

	// History panel
	Snapshots []historyRow
}

func (s *sitesController) workspaceHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	page := c.QueryParam("page")
	err = validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	all, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list pages", err)
	}
	pages, assetPaths := build.SplitFilesByKind(all)
	stylesheets := authorStylesheets(all)
	assets := s.collectWorkspaceAssets(ctx, slug, assetPaths)
	functions := collectFunctionNames(all)
	meta := s.build.ReadMeta(ctx, slug)

	currentTheme := s.readCurrentTheme(ctx, slug)
	snaps := s.listSnapshotRows(ctx, slug)

	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}

	building := c.QueryParam("building") == "1" || s.buildInFlight(slug)

	return s.render(c, "workspace", workspaceData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "workspace",
		},
		PageURL:          s.siteURL(c, slug, "/"+page),
		Page:             page,
		Pages:            pages,
		Stylesheets:      stylesheets,
		Assets:           assets,
		Functions:        functions,
		Flash:            c.QueryParam("flash"),
		Building:         building,
		CurrentTheme:     currentTheme,
		SlugJSON:         toJSONLiteral(slug),
		CurrentThemeJSON: toJSONLiteral(currentTheme),
		ThemesJSON:       toJSONLiteral(daisyThemes),
		Snapshots:        snaps,
	})
}

// authorStylesheets returns the stylesheets the site wrote for itself, so the
// owner can see and scope an edit to the file that actually governs the design.
// Excludes the generated app.css, which the platform overwrites on every build.
func authorStylesheets(all []string) []string {
	var sheets []string
	for _, f := range build.EditableFiles(all) {
		if strings.HasSuffix(f, ".css") {
			sheets = append(sheets, f)
		}
	}
	return sheets
}

// collectWorkspaceAssets returns the image rows for the workspace's image
// library — mirrors editHandler's per-asset alt-text lookup. Reads are cached
// via ARC so this only pays an S3 round-trip on cold paths.
func (s *Server) collectWorkspaceAssets(ctx context.Context, slug string, paths []string) []editAsset {
	out := make([]editAsset, 0, len(paths))
	for _, p := range paths {
		row := editAsset{Path: p}
		obj, err := s.store.Read(ctx, slug, p)
		if err == nil && obj != nil {
			row.Alt = obj.Metadata["alt"]
		} else if err != nil {
			slog.Warn("workspace.asset_meta", "slug", slug, "path", p, "err", err)
		}
		out = append(out, row)
	}
	return out
}

// readCurrentTheme pulls the data-theme attribute off index.html so the
// Themes panel can highlight the currently-applied theme. Defaults to
// "light" when the site has no theme attribute yet, matching themeStudio.
func (s *Server) readCurrentTheme(ctx context.Context, slug string) string {
	obj, err := s.store.Read(ctx, slug, "index.html")
	if err != nil || obj == nil || obj.Content == "" {
		return "light"
	}
	theme, _ := readThemeAttribute(obj.Content)
	if theme == "" {
		return "light"
	}
	return theme
}

// listSnapshotRows wraps snapshot.List() with the row formatting the history
// panel needs. Returns nil (not an error) when snapshots aren't configured,
// so the workspace still renders.
func (s *Server) listSnapshotRows(ctx context.Context, slug string) []historyRow {
	if s.snapshot == nil {
		return nil
	}
	snaps, err := s.snapshot.List(ctx, slug)
	if err != nil {
		slog.Warn("workspace.snapshots", "slug", slug, "err", err)
		return nil
	}
	rows := make([]historyRow, 0, len(snaps))
	for _, sn := range snaps {
		rows = append(rows, historyRow{
			Key:       sn.Key,
			Reason:    sn.Reason,
			FileCount: sn.FileCount,
			WhenLabel: humanizeAge(sn.Timestamp),
			WhenISO:   sn.Timestamp.Format(time.RFC3339),
			SizeLabel: humanizeBytes(sn.SizeBytes),
		})
	}
	return rows
}

// buildInFlight reports whether the events tracker shows an active run for
// slug. Used to set Building=true when the user refreshes mid-build without
// the ?building=1 query param.
func (s *Server) buildInFlight(slug string) bool {
	if s.events == nil {
		return false
	}
	st := s.events.Get(slug)
	return st != nil && events.IsActive(st.Status)
}

// toJSONLiteral marshals v to JSON and returns it as template.JS so the
// html/template engine emits it verbatim inside a <script> block. This lets
// templates assign server-supplied values directly to JS variables without
// an intermediate JSON.parse step.
func toJSONLiteral(v any) template.JS {
	b, err := json.Marshal(v)
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b) //nolint:gosec // values are JSON-marshaled, not user-controlled JS.
}
