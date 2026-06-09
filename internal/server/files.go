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
	Path string
	// Size is the human-readable label ("12 KB"). SizeBytes is the raw
	// value, exposed via data-size on the row so the client-side filter
	// can recompute the visible-total in the footer.
	Size      string
	SizeBytes int64
	Modified  string
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
	// Deletable controls whether the trash action appears for this row.
	// State sidecars and the legacy meta file are off-limits — they're
	// platform-managed, not user content.
	Deletable bool
	// IsHomepage swaps the confirm copy to warn the owner that deleting
	// index.html breaks their site.
	IsHomepage bool
}

type filesView struct {
	Chrome
	Rows      []fileRow
	TotalSize string
}

func (s *sitesController) filesHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
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
	var totalBytes int64
	for _, e := range entries {
		act := s.fileActionsFor(c, slug, e.Path)
		_, classifyErr := classifyUserPath(e.Path)
		rows = append(rows, fileRow{
			Path:       e.Path,
			Size:       formatSize(e.Size),
			SizeBytes:  e.Size,
			Modified:   e.LastModified.UTC().Format("2006-01-02 15:04"),
			EditURL:    act.EditURL,
			LinkURL:    act.OpenURL,
			LinkLabel:  act.OpenLabel,
			Deletable:  classifyErr == nil,
			IsHomepage: e.Path == "index.html",
		})
		totalBytes += e.Size
	}

	meta := s.build.ReadMeta(c.Request().Context(), slug)
	siteName := meta.Title
	if siteName == "" {
		siteName = slug
	}
	return s.render(c, "files", filesView{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  siteURL,
			Active:   "files",
		},
		Rows:      rows,
		TotalSize: formatSize(totalBytes),
	})
}

// actionsFor returns the (editURL, openURL, openLabel) triple for a file.
// HTML pages and server functions get an edit link to the appropriate
// editor; public-facing files also get an "open" link to the live URL.
// State sidecars get a "view data" link. Files with no useful action get
// empty strings — the template renders the path as plain text.
// FileActions is the set of links the files list renders for one entry: the
// in-app editor URL, the live "open" URL, and that link's label. Any field may
// be empty when the action doesn't apply to the file kind.
type FileActions struct {
	EditURL   string
	OpenURL   string
	OpenLabel string
}

func (s *Server) fileActionsFor(c *echo.Context, slug, path string) FileActions {
	switch {
	case strings.HasPrefix(path, "functions/") && strings.HasSuffix(path, ".js"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "functions/"), ".js")
		return FileActions{EditURL: "/edit/" + slug + "/function/" + name}
	case strings.HasPrefix(path, "_state/"):
		return FileActions{OpenURL: "/manage/" + slug, OpenLabel: "view data"}
	case strings.HasSuffix(path, ".html"):
		return FileActions{EditURL: "/workspace/" + slug + "?page=" + url.QueryEscape(path), OpenURL: s.siteURL(c, slug, "/"+path), OpenLabel: "open"}
	case strings.HasPrefix(path, "assets/"):
		return FileActions{OpenURL: s.siteURL(c, slug, "/"+path), OpenLabel: "open"}
	default:
		return FileActions{}
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
