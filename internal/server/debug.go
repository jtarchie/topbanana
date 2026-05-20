package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/bloomhollow/internal/editrec"
)

type debugRow struct {
	Key       string
	LogKey    string
	WhenLabel string
	WhenISO   string
}

type debugListData struct {
	Chrome
	Rows []debugRow
}

type debugToolRow struct {
	IndexLabel string
	Tool       string
	Phase      string
	Path       string
	Message    string
	WhenISO    string
}

type debugFileRow struct {
	Path            string
	Tool            string
	BeforeSize      int
	AfterSize       int
	BeforeSHA       string
	AfterSHA        string
	BeforeContent   string
	AfterContent    string
	BeforeTruncated bool
	AfterTruncated  bool
	Changed         bool
	Created         bool
	Deleted         bool
}

type debugDetailData struct {
	Chrome
	Key             string
	LogKey          string
	StartedAt       string
	FinishedAt      string
	Duration        string
	Model           string
	ReasoningEffort string
	UserPrompt      string
	Page            string
	SelectionLen    int
	FinalStatus     string
	Error           string
	ToolCalls       []debugToolRow
	FileChanges     []debugFileRow
	Empty           bool
}

func (s *Server) debugHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	rows, err := editrec.List(c.Request().Context(), s.store, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list transcripts", err)
	}

	out := make([]debugRow, 0, len(rows))
	for _, r := range rows {
		out = append(out, debugRow{
			Key:       r.Key,
			LogKey:    r.LogKey,
			WhenLabel: humanizeAge(r.Timestamp),
			WhenISO:   r.Timestamp.Format(time.RFC3339),
		})
	}

	return s.render(c, "debug", debugListData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: s.siteNameOrSlug(c.Request().Context(), slug),
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "debug",
		},
		Rows: out,
	})
}

func (s *Server) debugDetailHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	key := c.QueryParam("key")
	err = validateTranscriptKey(slug, key)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	t, err := editrec.Read(c.Request().Context(), s.store, key)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read transcript", err)
	}

	data := debugDetailData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: s.siteNameOrSlug(c.Request().Context(), slug),
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "debug",
		},
		Key:             key,
		LogKey:          t.LogKey,
		Model:           t.Model,
		ReasoningEffort: t.ReasoningEffort,
		UserPrompt:      t.UserPrompt,
		Page:            t.Page,
		SelectionLen:    t.SelectionLen,
		FinalStatus:     t.FinalStatus,
		Error:           t.Error,
	}
	if t.StartedAt.IsZero() && len(t.ToolCalls) == 0 && len(t.FileChanges) == 0 {
		data.Empty = true
		return s.render(c, "debug_edit", data)
	}

	data.StartedAt = t.StartedAt.Format(time.RFC3339)
	if !t.FinishedAt.IsZero() {
		data.FinishedAt = t.FinishedAt.Format(time.RFC3339)
		if !t.StartedAt.IsZero() {
			data.Duration = t.FinishedAt.Sub(t.StartedAt).Round(time.Millisecond).String()
		}
	}

	data.ToolCalls = make([]debugToolRow, 0, len(t.ToolCalls))
	for i, tc := range t.ToolCalls {
		data.ToolCalls = append(data.ToolCalls, debugToolRow{
			IndexLabel: strconv.Itoa(i),
			Tool:       tc.Tool,
			Phase:      tc.Phase,
			Path:       tc.Path,
			Message:    tc.Message,
			WhenISO:    tc.Timestamp.Format(time.RFC3339),
		})
	}

	data.FileChanges = make([]debugFileRow, 0, len(t.FileChanges))
	for _, fc := range t.FileChanges {
		row := debugFileRow{
			Path:            fc.Path,
			Tool:            fc.Tool,
			BeforeSize:      fc.BeforeSize,
			AfterSize:       fc.AfterSize,
			BeforeSHA:       shortSHA(fc.BeforeSHA256),
			AfterSHA:        shortSHA(fc.AfterSHA256),
			BeforeContent:   fc.BeforeContent,
			AfterContent:    fc.AfterContent,
			BeforeTruncated: fc.BeforeTruncated,
			AfterTruncated:  fc.AfterTruncated,
			Created:         fc.BeforeSize == 0 && fc.AfterSize > 0,
			Deleted:         fc.BeforeSize > 0 && fc.AfterSize == 0,
		}
		row.Changed = fc.BeforeSHA256 != fc.AfterSHA256
		data.FileChanges = append(data.FileChanges, row)
	}

	return s.render(c, "debug_edit", data)
}

// debugCacheCheckHandler fetches the same file two ways — direct S3 read and
// HTTP GET against the public URL — so the user can tell whether the served
// bytes match what's in storage. CDN/browser caching is the prime suspect
// when "agent ran, file looks fixed in storage, but the live site still
// shows the old version."
func (s *Server) debugCacheCheckHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	page := c.QueryParam("path")
	if page == "" {
		page = "index.html"
	}
	err = validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	obj, err := s.store.Read(ctx, slug, page)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read s3", err)
	}

	servedURL := s.siteURL(c, slug, "/"+page)
	servedSHA, servedSize, servedHeaders, servedErr := fetchServed(ctx, servedURL)

	out := map[string]any{
		"slug":        slug,
		"path":        page,
		"served_url":  servedURL,
		"s3_size":     len(obj.Content),
		"s3_sha256":   sha256Hex(obj.Content),
		"s3_etag":     obj.ETag,
		"served_size": servedSize,
		"served_sha":  servedSHA,
		"served_hdrs": servedHeaders,
	}
	if servedErr != nil {
		out["error"] = servedErr.Error()
		out["verdict"] = "fetch-failed"
		return c.JSON(http.StatusOK, out) //nolint:wrapcheck
	}
	switch {
	case len(obj.Content) == 0 && servedSize == 0:
		out["verdict"] = "not-found"
	case sha256Hex(obj.Content) == servedSHA:
		out["verdict"] = "match"
	default:
		out["verdict"] = "stale-served"
	}
	return c.JSON(http.StatusOK, out) //nolint:wrapcheck
}

// cacheCheckTimeout caps the served-URL fetch so a wedged backend can't hang
// the admin's debug request.
const cacheCheckTimeout = 5 * time.Second

func fetchServed(ctx context.Context, servedURL string) (sha string, size int, headers map[string]string, err error) {
	fctx, cancel := context.WithTimeout(ctx, cacheCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fctx, http.MethodGet, servedURL, nil)
	if err != nil {
		return "", 0, nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("User-Agent", "bloomhollow-cache-check/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, nil, fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", 0, nil, fmt.Errorf("read body: %w", err)
	}
	headers = map[string]string{
		"Cache-Control": resp.Header.Get("Cache-Control"),
		"ETag":          resp.Header.Get("ETag"),
		"Content-Type":  resp.Header.Get("Content-Type"),
		"Age":           resp.Header.Get("Age"),
	}
	return sha256Hex(string(body)), len(body), headers, nil
}

func sha256Hex(content string) string {
	if content == "" {
		return ""
	}
	h := sha256.Sum256([]byte(content))
	return hex.EncodeToString(h[:])
}

// validateTranscriptKey enforces the bucket-key prefix so a user can't read
// an arbitrary object by passing a hand-crafted key.
func validateTranscriptKey(slug, key string) error {
	if key == "" {
		return errors.New("key is required")
	}
	prefix := editrec.Prefix + slug + "/"
	if !strings.HasPrefix(key, prefix) {
		return fmt.Errorf("key %q does not belong to slug %q", key, slug)
	}
	base := filepath.Base(key)
	if !strings.HasSuffix(base, ".json") {
		return fmt.Errorf("key %q is not a transcript", key)
	}
	if strings.Contains(key, "..") {
		return fmt.Errorf("key %q contains traversal", key)
	}
	return nil
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
