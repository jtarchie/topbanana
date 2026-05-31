package server

import (
	"context"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
)

// workspaceData backs the unified design surface at /workspace/:slug. It
// folds together what used to be three separate pages — Edit, Appearance,
// and Version history — by carrying the same per-page edit data the old
// editHandler exposed plus the theme picker and snapshot list as inline
// panels.
type workspaceData struct {
	Chrome
	PageURL   string
	Page      string
	Pages     []string
	Assets    []editAsset
	Functions []string
	Flash     string

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

func (s *Server) workspaceHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
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
	return st != nil && (st.Status == "building" || st.Status == "linting" || st.Status == "retry")
}
