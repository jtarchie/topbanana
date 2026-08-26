package server

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// The workspace run feed: the JSON behind the conversation panel. Each entry
// is one user-initiated agent run (build or edit) — the prompt the user typed
// and the verdict it produced. The panel exists because completion alone is
// not an answer: a run that finishes without changing anything must say so,
// and say why, or the user retries blind (the 2026-08-25 tinytools incident).

// runFeedLimit caps the feed. Enough to show the current session's
// back-and-forth; /debug holds the full retained history.
const runFeedLimit = 8

type runFeedEntry struct {
	WhenISO   string   `json:"when_iso"`
	WhenLabel string   `json:"when_label"`
	Prompt    string   `json:"prompt"`
	Status    string   `json:"status"`
	Files     []string `json:"files"`
	// Key deep-links the entry to /debug/{slug}/edit?key=... ("see exactly
	// what changed").
	Key string `json:"key,omitempty"`
	// FinalMessage is the agent's closing text — rendered on runs that
	// changed nothing, where it is the only explanation the user gets.
	FinalMessage string `json:"final_message,omitempty"`
	// UndoKey is set on the newest run only: the pre-run snapshot to restore
	// via PUT /history/{slug}. Older runs deliberately get none — restoring
	// an older snapshot would silently undo every run after it too, which is
	// a version-history decision, not a one-click undo.
	UndoKey string `json:"undo_key,omitempty"`
}

func (s *sitesController) runsFeedHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	runs := s.build.RecentRuns(ctx, slug, runFeedLimit)
	out := make([]runFeedEntry, 0, len(runs))
	for _, r := range runs {
		files := r.Files
		if files == nil {
			files = []string{}
		}
		out = append(out, runFeedEntry{
			WhenISO:      r.At.UTC().Format(time.RFC3339),
			WhenLabel:    humanizeAge(r.At),
			Prompt:       r.Prompt,
			Status:       r.Status,
			Files:        files,
			Key:          r.Key,
			FinalMessage: r.FinalMessage,
		})
	}
	if len(out) > 0 {
		out[0].UndoKey = s.latestRunSnapshotKey(c, slug)
	}

	return c.JSON(http.StatusOK, map[string]any{"runs": out}) //nolint:wrapcheck
}

// latestRunSnapshotKey returns the newest pre-run snapshot for the slug, or
// "". Build/edit snapshots are taken at run start, so the newest one is the
// state just before the newest run — the target for its Undo. Settings and
// other snapshot reasons are skipped: restoring one of those from an "undo
// this edit" button would surprise.
func (s *sitesController) latestRunSnapshotKey(c *echo.Context, slug string) string {
	if s.snapshot == nil {
		return ""
	}
	snaps, err := s.snapshot.List(c.Request().Context(), slug)
	if err != nil {
		return ""
	}
	var best *snapshot.Snapshot
	for i := range snaps {
		sn := &snaps[i]
		if sn.Reason != snapshot.ReasonBuild && sn.Reason != snapshot.ReasonEdit {
			continue
		}
		if best == nil || sn.Timestamp.After(best.Timestamp) {
			best = sn
		}
	}
	if best == nil {
		return ""
	}
	return best.Key
}
