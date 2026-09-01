package server

import (
	"context"
	"encoding/json"
	"html/template"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
)

// Shared state readers behind the workspace at /workspace/:slug (rendered by
// canvasHandler) and the surfaces that redirect into it. They live apart from
// canvas.go because each answers a question the canvas asks about the site's
// current state rather than about the canvas itself.

// authorStylesheets returns the stylesheets the site wrote for itself, so the
// owner can see and scope an edit to the file that actually governs the
// design. Excludes the generated app.css: EditableFiles filters to what the
// agent's own path gates accept, and that gate refuses app.css because the
// platform overwrites it on every build.
//
// This matters most on a bring-your-own-CSS site (see build.OwnStylesheets),
// where the sheet *is* the design and lint errors name it by path — without a
// listed entry point the owner has nothing to scope an edit to.
func authorStylesheets(all []string) []string {
	var sheets []string
	for _, f := range build.EditableFiles(all) {
		if strings.HasSuffix(f, ".css") {
			sheets = append(sheets, f)
		}
	}
	return sheets
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
// slug. Used to reattach the canvas to a run started elsewhere (MCP, another
// tab) instead of rendering an idle composer whose first submit 409s.
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
