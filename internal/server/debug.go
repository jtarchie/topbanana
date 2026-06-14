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
	"github.com/pmezard/go-difflib/difflib"

	"github.com/jtarchie/topbanana/internal/editrec"
)

// diffContextLines is how many unchanged lines surround each change hunk,
// matching the default GitHub/`diff -u` context window.
const diffContextLines = 3

// debugController serves the per-site "what did the agent actually do" surface:
// the build/edit transcript list, a single transcript's detail, and the
// served-vs-stored cache check. All reads, all owner-scoped.
type debugController struct{ *Server }

func (s *debugController) register(g *echo.Group, owns echo.MiddlewareFunc) {
	g.GET("/debug/:slug", s.debugHandler, owns)
	g.GET("/debug/:slug/edit", s.debugDetailHandler, owns)
	g.GET("/debug/:slug/cache-check", s.debugCacheCheckHandler, owns)
}

type debugRow struct {
	Key       string
	LogKey    string
	WhenLabel string
	WhenISO   string
	// Status / Duration / ToolCount are enriched per-row from the underlying
	// transcript JSON so the index page can render a real table the admin
	// can scan without click-per-row. N is bounded by editrec.Trim retention,
	// so reading each transcript is acceptable for an admin-only page.
	Status    string
	Duration  string
	ToolCount int
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
	// Files holds the file mutations attributed to this tool call (via
	// FileChange.ToolCallIndex), so the diff renders inline in the timeline
	// instead of in a detached section. Usually 0 or 1 entry.
	Files []debugFileRow
}

// diffLine is one rendered row of a unified diff. Kind is "hunk" (the
// `@@ … @@` header), "add", "del", or "context". Old/New are 1-based line
// numbers (0 when the side has no line, e.g. New on a deletion).
type diffLine struct {
	Kind string
	Old  int
	New  int
	Text string
}

type debugFileRow struct {
	// Slug is carried on the row so the inline "filediff" partial is
	// self-contained (it can't reach the page root for the cache-check button).
	Slug            string
	Path            string
	Tool            string
	BeforeSize      int
	AfterSize       int
	BeforeSHA       string
	AfterSHA        string
	BeforeTruncated bool
	AfterTruncated  bool
	Changed         bool
	Created         bool
	Deleted         bool
	// WhitespaceOnly is set when the file's bytes changed but the diff is
	// empty once whitespace is normalized away — surfaced as a note instead
	// of an empty diff so the row doesn't read as "no change".
	WhitespaceOnly bool
	Diff           []diffLine
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
	Template        string
	UserPrompt      string
	SystemPrompt    string
	Page            string
	SelectionLen    int
	FinalStatus     string
	Error           string
	Usage           editrec.Usage
	HasUsage        bool
	CacheHitPct     string
	ToolCalls       []debugToolRow
	// FileChangeCount is the total mutations recorded, used to show the
	// "nothing changed" diagnostic when tool calls ran but wrote nothing.
	FileChangeCount int
	// OrphanFileChanges holds mutations whose ToolCallIndex didn't resolve to
	// a tool call (defensive — practically never happens). Rendered in a
	// fallback section so a malformed transcript never silently drops a diff.
	OrphanFileChanges []debugFileRow
	Empty             bool
}

func (s *debugController) debugHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}

	rows, err := editrec.List(c.Request().Context(), s.store, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list transcripts", err)
	}

	ctx := c.Request().Context()
	out := make([]debugRow, 0, len(rows))
	for _, r := range rows {
		row := debugRow{
			Key:       r.Key,
			LogKey:    r.LogKey,
			WhenLabel: humanizeAge(r.Timestamp),
			WhenISO:   r.Timestamp.Format(time.RFC3339),
		}
		// Enrich per-row from the transcript JSON so the table can carry
		// status / duration / tool-count. N is bounded by editrec.Trim
		// retention; on a Read failure we render the row anyway with the
		// fields we already have rather than failing the page.
		t, readErr := editrec.Read(ctx, s.store, r.Key)
		if readErr == nil {
			row.Status = t.FinalStatus
			row.ToolCount = len(t.ToolCalls)
			if !t.FinishedAt.IsZero() && !t.StartedAt.IsZero() {
				row.Duration = t.FinishedAt.Sub(t.StartedAt).Round(time.Millisecond).String()
			}
		}
		out = append(out, row)
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

func (s *debugController) debugDetailHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
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
		Template:        t.Template,
		UserPrompt:      t.UserPrompt,
		SystemPrompt:    t.SystemPrompt,
		Page:            t.Page,
		SelectionLen:    t.SelectionLen,
		FinalStatus:     t.FinalStatus,
		Error:           t.Error,
		Usage:           t.Usage,
	}
	if t.Usage.Responses > 0 {
		data.HasUsage = true
		if t.Usage.Prompt > 0 {
			data.CacheHitPct = fmt.Sprintf("%.0f%%", 100*float64(t.Usage.Cached)/float64(t.Usage.Prompt))
		}
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

	data.ToolCalls = make([]debugToolRow, len(t.ToolCalls))
	for i, tc := range t.ToolCalls {
		data.ToolCalls[i] = debugToolRow{
			IndexLabel: strconv.Itoa(i),
			Tool:       tc.Tool,
			Phase:      tc.Phase,
			Path:       tc.Path,
			Message:    tc.Message,
			WhenISO:    tc.Timestamp.Format(time.RFC3339),
		}
	}

	// Attach each mutation to the tool call that produced it so the diff
	// renders inline in the timeline. ToolCallIndex points at the tool's
	// "done" event; anything out of range falls back to its own section.
	data.FileChangeCount = len(t.FileChanges)
	for _, fc := range t.FileChanges {
		row := buildFileRow(slug, fc)
		if fc.ToolCallIndex >= 0 && fc.ToolCallIndex < len(data.ToolCalls) {
			tc := &data.ToolCalls[fc.ToolCallIndex]
			tc.Files = append(tc.Files, row)
			continue
		}
		data.OrphanFileChanges = append(data.OrphanFileChanges, row)
	}

	return s.render(c, "debug_edit", data)
}

// debugCacheCheckHandler fetches the same file two ways — direct S3 read and
// HTTP GET against the public URL — so the user can tell whether the served
// bytes match what's in storage. CDN/browser caching is the prime suspect
// when "agent ran, file looks fixed in storage, but the live site still
// shows the old version."
func (s *debugController) debugCacheCheckHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
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
	req.Header.Set("User-Agent", "topbanana-cache-check/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", 0, nil, fmt.Errorf("fetch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// resp is guaranteed non-nil here: http.Client.Do's contract is
	// (resp != nil) iff (err == nil), and the line above already
	// dereferenced resp.Body without panicking.
	body, err := io.ReadAll(resp.Body) //nolint:nilaway // see comment.
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

// buildFileRow turns a recorded FileChange into the view row, computing the
// whitespace-insensitive diff for the timeline. WhitespaceOnly catches the
// case where the bytes changed (different sha) but every change is whitespace,
// so the diff comes back empty.
func buildFileRow(slug string, fc editrec.FileChange) debugFileRow {
	row := debugFileRow{
		Slug:            slug,
		Path:            fc.Path,
		Tool:            fc.Tool,
		BeforeSize:      fc.BeforeSize,
		AfterSize:       fc.AfterSize,
		BeforeSHA:       shortSHA(fc.BeforeSHA256),
		AfterSHA:        shortSHA(fc.AfterSHA256),
		BeforeTruncated: fc.BeforeTruncated,
		AfterTruncated:  fc.AfterTruncated,
		Created:         fc.BeforeSize == 0 && fc.AfterSize > 0,
		Deleted:         fc.BeforeSize > 0 && fc.AfterSize == 0,
	}
	row.Changed = fc.BeforeSHA256 != fc.AfterSHA256
	row.Diff = buildDiff(fc.BeforeContent, fc.AfterContent)
	row.WhitespaceOnly = row.Changed && len(row.Diff) == 0 && !row.Created && !row.Deleted
	return row
}

// buildDiff produces a GitHub-style unified diff between before and after.
// Lines are matched on their whitespace-stripped form so re-indentation,
// trailing-space churn, and reflowed tags don't register as changes — this
// mirrors GitHub's "ignore whitespace" toggle (git diff -w). The rendered
// text stays verbatim. Returns nil when the two sides are equal once
// whitespace is ignored.
func buildDiff(before, after string) []diffLine {
	if before == after {
		return nil
	}
	aLines := splitLines(before)
	bLines := splitLines(after)
	// autoJunk off: it skips "popular" lines on long inputs, which produces
	// noisier diffs on repetitive HTML (many `</div>`); we want the minimal one.
	m := difflib.NewMatcherWithJunk(normalizeLines(aLines), normalizeLines(bLines), false, nil)

	var out []diffLine
	for _, group := range m.GetGroupedOpCodes(diffContextLines) {
		first, last := group[0], group[len(group)-1]
		out = append(out, diffLine{
			Kind: "hunk",
			Text: fmt.Sprintf("@@ -%s +%s @@", formatRange(first.I1, last.I2), formatRange(first.J1, last.J2)),
		})
		for _, op := range group {
			switch op.Tag {
			case 'e':
				for i, j := op.I1, op.J1; i < op.I2; i, j = i+1, j+1 {
					out = append(out, diffLine{Kind: "context", Old: i + 1, New: j + 1, Text: aLines[i]})
				}
			case 'd':
				for i := op.I1; i < op.I2; i++ {
					out = append(out, diffLine{Kind: "del", Old: i + 1, Text: aLines[i]})
				}
			case 'i':
				for j := op.J1; j < op.J2; j++ {
					out = append(out, diffLine{Kind: "add", New: j + 1, Text: bLines[j]})
				}
			case 'r':
				for i := op.I1; i < op.I2; i++ {
					out = append(out, diffLine{Kind: "del", Old: i + 1, Text: aLines[i]})
				}
				for j := op.J1; j < op.J2; j++ {
					out = append(out, diffLine{Kind: "add", New: j + 1, Text: bLines[j]})
				}
			}
		}
	}
	return out
}

// splitLines splits content into lines, dropping a single trailing newline so
// a normal "ends in \n" file doesn't render a spurious blank final line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// normalizeLines strips all whitespace from each line to form the match key
// (matching only — display still uses the original lines). Join with "" so two
// lines that differ solely in whitespace, including whitespace present on one
// side and absent on the other, compare equal — the git diff -w rule.
func normalizeLines(lines []string) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = strings.Join(strings.Fields(l), "")
	}
	return out
}

// formatRange renders the line span for a unified-diff hunk header, mirroring
// `diff -u`: a 1-line span is just the start; an empty span uses the
// before-the-range convention (`start,0`).
func formatRange(start, stop int) string {
	length := stop - start
	switch length {
	case 1:
		return strconv.Itoa(start + 1)
	case 0:
		return strconv.Itoa(start) + ",0"
	default:
		return strconv.Itoa(start+1) + "," + strconv.Itoa(length)
	}
}
