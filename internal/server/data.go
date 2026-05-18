package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"
)

// dataRow is one submission row in the inline table on /manage/:slug and in
// the CSV export at /data/:slug?format=csv.
type dataRow struct {
	Key    string
	Values []string
}

// dataHandler serves the CSV download for a site's KV submissions. The HTML
// rendering of the same data lives inline on /manage/:slug; legacy GETs to
// /data/:slug without the format query are redirected there.
func (s *Server) dataHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if c.QueryParam("format") != "csv" {
		return c.Redirect(http.StatusFound, "/manage/"+slug) //nolint:wrapcheck
	}

	cols, rows, err := s.collectSubmissions(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "load state", err)
	}
	return writeSubmissionsCSV(c, slug, cols, rows)
}

// collectSubmissions loads the slug's KV snapshot and returns the column list
// and rows ready for rendering. Returns an empty list (not an error) when the
// state backend is unconfigured or the site has no data yet.
func (s *Server) collectSubmissions(ctx context.Context, slug string) ([]string, []dataRow, error) {
	if s.state == nil {
		return nil, nil, nil
	}
	snap, err := s.state.Load(ctx, slug)
	if err != nil {
		return nil, nil, fmt.Errorf("state load: %w", err)
	}

	type entry struct {
		key  string
		data map[string]any
	}
	entries := make([]entry, 0, len(snap.Data))
	fieldSet := map[string]struct{}{}
	for k, v := range snap.Data {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		entries = append(entries, entry{key: k, data: m})
		for f := range m {
			fieldSet[f] = struct{}{}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		return entries[i].key < entries[j].key
	})

	// Stable column order: alphabetical, with `ts` pinned to the end so the
	// timestamp lives at the right edge of every table.
	cols := make([]string, 0, len(fieldSet))
	hasTs := false
	for f := range fieldSet {
		if f == "ts" {
			hasTs = true
			continue
		}
		cols = append(cols, f)
	}
	sort.Strings(cols)
	if hasTs {
		cols = append(cols, "ts")
	}

	rows := make([]dataRow, 0, len(entries))
	for _, e := range entries {
		values := make([]string, len(cols))
		for i, col := range cols {
			values[i] = formatCell(e.data[col], col)
		}
		rows = append(rows, dataRow{Key: e.key, Values: values})
	}
	return cols, rows, nil
}

// formatCell turns a KV value into a display string. Numeric `ts` is parsed as
// Unix milliseconds and rendered RFC3339; integer-valued floats drop their
// trailing `.0`; everything else falls through to JSON for unambiguous nesting.
func formatCell(v any, col string) string {
	if v == nil {
		return ""
	}
	if col == "ts" {
		ms, ok := asInt64(v)
		if ok {
			return time.UnixMilli(ms).UTC().Format(time.RFC3339)
		}
	}
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case float64:
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10)
		}
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return fmt.Sprintf("%v", v)
		}
		return string(b)
	}
}

// asInt64 coerces the JSON-decoded numeric forms we might see in snapshot
// data. encoding/json defaults to float64; json.Number arises if a caller ever
// switches the decoder to UseNumber.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
	case int:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true
		}
	}
	return 0, false
}

func writeSubmissionsCSV(c *echo.Context, slug string, cols []string, rows []dataRow) error {
	resp := c.Response()
	resp.Header().Set("Content-Type", "text/csv; charset=utf-8")
	resp.Header().Set("Content-Disposition", `attachment; filename="`+slug+`-submissions.csv"`)
	resp.WriteHeader(http.StatusOK)

	w := csv.NewWriter(resp)
	header := append([]string{"key"}, cols...)
	err := w.Write(header)
	if err != nil {
		return fmt.Errorf("write csv header: %w", err)
	}
	for _, row := range rows {
		record := append([]string{row.Key}, row.Values...)
		err = w.Write(record)
		if err != nil {
			return fmt.Errorf("write csv row: %w", err)
		}
	}
	w.Flush()
	err = w.Error()
	if err != nil {
		return fmt.Errorf("flush csv: %w", err)
	}
	return nil
}
