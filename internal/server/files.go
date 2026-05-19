package server

import (
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/labstack/echo/v5"
)

// fileRow is one row of the explorer table.
type fileRow struct {
	Path     string
	Size     string
	Modified string
	// EditURL points to the editor surface for this file when one exists —
	// /workspace/:slug?page=… for HTML pages, /edit/:slug/function/:name for
	// server functions. Empty for files that aren't user-editable (assets,
	// state sidecars, etc.).
	EditURL string
	// LinkURL is the "open" or "view" link — opens the live page on the
	// subdomain for HTML/asset files, lands on /manage/:slug for state files.
	// Empty when no view action exists.
	LinkURL   string
	LinkLabel string
}

type filesView struct {
	Slug     string
	SiteName string // consumed by the shared brand partial's breadcrumb
	SiteURL  string
	Active   string
	Rows     []fileRow
}

func (s *Server) filesHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	entries, err := s.store.ListWithMeta(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list files", err)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})

	siteURL := s.siteURL(c, slug, "/")
	rows := make([]fileRow, 0, len(entries))
	for _, e := range entries {
		editURL, openURL, openLabel := actionsFor(c, s, slug, e.Path)
		rows = append(rows, fileRow{
			Path:      e.Path,
			Size:      formatSize(e.Size),
			Modified:  e.LastModified.UTC().Format("2006-01-02 15:04"),
			EditURL:   editURL,
			LinkURL:   openURL,
			LinkLabel: openLabel,
		})
	}

	meta := s.build.ReadMeta(c.Request().Context(), slug)
	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}
	return s.render(c, "files", filesView{
		Slug:     slug,
		SiteName: siteName,
		SiteURL:  siteURL,
		Active:   "files",
		Rows:     rows,
	})
}

// actionsFor returns the (editURL, openURL, openLabel) triple for a file.
// HTML pages and server functions get an edit link to the appropriate
// editor; public-facing files also get an "open" link to the live URL.
// State sidecars get a "view data" link. Files with no useful action get
// empty strings — the template renders the path as plain text.
func actionsFor(c *echo.Context, s *Server, slug, path string) (editURL, openURL, openLabel string) {
	switch {
	case strings.HasPrefix(path, "functions/") && strings.HasSuffix(path, ".js"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "functions/"), ".js")
		return "/edit/" + slug + "/function/" + name, "", ""
	case strings.HasPrefix(path, "_state/"):
		return "", "/manage/" + slug, "view data"
	case strings.HasSuffix(path, ".html"):
		return "/workspace/" + slug + "?page=" + url.QueryEscape(path), s.siteURL(c, slug, "/"+path), "open"
	case strings.HasPrefix(path, "assets/"):
		return "", s.siteURL(c, slug, "/"+path), "open"
	default:
		return "", "", ""
	}
}

// formatSize renders bytes as a short human-readable string. KB/MB use base
// 1024 to match S3 console conventions.
func formatSize(n int64) string {
	switch {
	case n < 1024:
		return strconv.FormatInt(n, 10) + " B"
	case n < 1024*1024:
		return strconv.FormatFloat(float64(n)/1024, 'f', 1, 64) + " KB"
	default:
		return strconv.FormatFloat(float64(n)/(1024*1024), 'f', 1, 64) + " MB"
	}
}
