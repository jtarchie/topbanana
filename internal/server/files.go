package server

import (
	"net/http"
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
	// LinkURL is empty when nothing actionable exists for the file. The
	// template renders the path as plain text in that case.
	LinkURL   string
	LinkLabel string
}

type filesView struct {
	Slug    string
	SiteURL string
	Rows    []fileRow
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
		url, label := actionFor(c, s, slug, e.Path)
		rows = append(rows, fileRow{
			Path:      e.Path,
			Size:      formatSize(e.Size),
			Modified:  e.LastModified.UTC().Format("2006-01-02 15:04"),
			LinkURL:   url,
			LinkLabel: label,
		})
	}

	return s.render(c, "files", filesView{
		Slug:    slug,
		SiteURL: siteURL,
		Rows:    rows,
	})
}

// actionFor returns the most useful link for a given file. Publicly-served
// files (pages + uploads) link to the live subdomain. Functions and state get
// their existing admin views. Sidecars / state ETag files have no action.
func actionFor(c *echo.Context, s *Server, slug, path string) (string, string) {
	switch {
	case strings.HasPrefix(path, "functions/") && strings.HasSuffix(path, ".js"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "functions/"), ".js")
		return "/edit/" + slug + "/function/" + name, "view source"
	case strings.HasPrefix(path, "_state/"):
		return "/data/" + slug, "view data"
	case strings.HasSuffix(path, ".html"):
		return s.siteURL(c, slug, "/"+path), "open"
	case strings.HasPrefix(path, "assets/"):
		return s.siteURL(c, slug, "/"+path), "open"
	default:
		return "", ""
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
