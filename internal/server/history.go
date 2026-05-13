package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

type historyRow struct {
	Key       string
	Reason    string
	FileCount int
	WhenLabel string
	WhenISO   string
	SizeLabel string
}

type historyData struct {
	Slug      string
	Domain    string
	Port      string
	Snapshots []historyRow
	Flash     string
}

func (s *Server) historyHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if s.snapshot == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "snapshots are not configured")
	}

	snaps, err := s.snapshot.List(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list snapshots", err)
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

	return s.render(c, "history", historyData{
		Slug:      slug,
		Domain:    s.domain,
		Port:      s.port,
		Snapshots: rows,
		Flash:     c.QueryParam("flash"),
	})
}

func (s *Server) historyRestoreHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if s.snapshot == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "snapshots are not configured")
	}
	key := c.FormValue("key")
	if key == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "key is required")
	}

	err = s.snapshot.Restore(c.Request().Context(), slug, key)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "restore snapshot", err)
	}
	slog.Info("snapshot.restore", "slug", slug, "key", key)
	return c.Redirect(http.StatusSeeOther, "/history/"+slug+"?flash="+urlEscape("Restored. Current state was auto-snapshotted as pre-restore.")) //nolint:wrapcheck
}

func (s *Server) historyDeleteHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if s.snapshot == nil {
		return echo.NewHTTPError(http.StatusServiceUnavailable, "snapshots are not configured")
	}
	key := c.FormValue("key")
	if key == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "key is required")
	}

	err = s.snapshot.Delete(c.Request().Context(), slug, key)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete snapshot", err)
	}
	slog.Info("snapshot.delete", "slug", slug, "key", key)
	return c.Redirect(http.StatusSeeOther, "/history/"+slug+"?flash="+urlEscape("Snapshot deleted.")) //nolint:wrapcheck
}

// humanizeAge renders timestamps relative to now ("3m ago") with an absolute
// fallback for anything older than a day.
func humanizeAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2, 2006 15:04")
	}
}

// humanizeBytes formats a byte count with binary units.
func humanizeBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KiB", float64(n)/kb)
	default:
		return fmt.Sprintf("%.2f MiB", float64(n)/mb)
	}
}

// urlEscape produces a query-safe value for the flash message redirect.
// Echo's Redirect doesn't take query params separately so the message is
// embedded in the URL directly.
func urlEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ':
			b.WriteRune('+')
		default:
			fmt.Fprintf(&b, "%%%02X", r)
		}
	}
	return b.String()
}
