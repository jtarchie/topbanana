package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
)

// historyRow is the per-snapshot row rendered in the workspace's history
// side panel. Built from snapshot.Snapshot with formatting suitable for the
// user-facing timeline.
type historyRow struct {
	Key       string
	Reason    string
	FileCount int
	WhenLabel string
	WhenISO   string
	SizeLabel string
}

func (s *sitesController) historyRestoreHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
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
	slog.Info("snapshot.restore", "slug", slug, "key", key, "user", callerEmail(c))
	// Sentinel, not a sentence: the workspace maps known codes to copy. Free
	// text through ?flash= would let any crafted link print its own words in
	// the workspace's trusted success banner.
	return c.Redirect(http.StatusSeeOther, "/workspace/"+slug+"?flash=restored") //nolint:wrapcheck
}

func (s *sitesController) historyDeleteHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
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
	slog.Info("snapshot.delete", "slug", slug, "key", key, "user", callerEmail(c))
	return c.Redirect(http.StatusSeeOther, "/workspace/"+slug+"?flash=snapshot-deleted") //nolint:wrapcheck
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
//
// Iterates BYTES, not runes: percent-encoding is defined over octets, so
// ranging by rune and formatting the code point with %02X turned an em dash
// (U+2014) into "%2014" — a space followed by a literal "14" once the browser
// decoded it. Several flash strings carry em dashes and typographic quotes.
func urlEscape(s string) string {
	var b strings.Builder
	for i := range len(s) {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
			b.WriteByte(c)
		case c == ' ':
			b.WriteByte('+')
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
