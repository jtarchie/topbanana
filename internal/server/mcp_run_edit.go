package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
)

// The MCP surface is deliberately direct-author — the caller IS an agent, so
// it edits files itself. run_edit is the one exception: it invokes the
// platform's own build agent, the same path the workspace edit box takes
// (history block, seeded prefetch, styling regime, lint loop, CSS compile).
// It exists so the edit pipeline can be exercised and verified end-to-end
// from outside the web UI — a platform operator asking "does the agent do
// the right thing with this prompt on this site?" gets the production
// answer, transcript included, without a browser session.

const (
	// runEditDefaultWait / runEditMaxWait bound how long the tool blocks on
	// the build before handing back a "still running" answer. Edits on the
	// configured models finish well inside the default; the cap keeps a
	// wedged build from pinning an MCP request until the client gives up.
	runEditDefaultWait = 180 * time.Second
	runEditMaxWait     = 300 * time.Second
	runEditPollEvery   = time.Second
)

type runEditInput struct {
	Slug        string `json:"slug"                   jsonschema:"The site slug to edit"`
	Prompt      string `json:"prompt"                 jsonschema:"The change request, in the site owner's words — exactly what would be typed into the workspace edit box"`
	Page        string `json:"page,omitempty"         jsonschema:"Optional page to scope the edit to (e.g. index.html). Empty means site-wide."`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"Seconds to wait for the run to finish before returning (default 180, max 300). A run still going returns status building; check it later with list_runs / get_run_transcript."`
}

func (s *Server) registerRunEdit(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "run_edit",
		Description: "Run the platform's build agent (an LLM) against a site with an edit prompt — the same pipeline as the workspace edit box, including prior-request history, site prefetch, the styling regime, the lint loop, and the CSS compile. Waits for the run and returns its status, the files it changed, and the agent's final message, plus the transcript key for get_run_transcript. Costs LLM tokens and takes tens of seconds; for direct authoring prefer edit_file/write_file. The agent may pause to ask a clarifying question — over MCP nobody can answer, so after 5 minutes it proceeds with its own recommendation; keep prompts unambiguous.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in runEditInput) (*mcp.CallToolResult, any, error) {
		user, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}

		prompt := strings.TrimSpace(in.Prompt)
		if prompt == "" {
			return nil, nil, mcpPlainErr(errors.New("prompt is required"))
		}
		if len(prompt) > maxPromptBytes {
			return nil, nil, mcpPlainErr(fmt.Errorf("prompt is too long (max %d bytes)", maxPromptBytes))
		}
		err = validatePage(in.Page)
		if err != nil {
			return nil, nil, mcpPlainErr(err)
		}
		if s.buildInFlight(in.Slug) {
			return nil, nil, mcpPlainErr(errors.New("a build is already in progress for this site — wait for it to finish"))
		}

		meta := s.build.ReadMeta(ctx, in.Slug)
		tmpl := build.EffectiveTemplate(meta)
		tiers := s.effectiveTiersFor(user)
		// The recorder stamps its StartedAt inside Start's goroutine, strictly
		// after this read, so now() is already a correct lower bound — no
		// slack, which would let a just-finished prior run's transcript be
		// misattributed to this call.
		startedAfter := time.Now().UTC()
		// EffectiveTemplate wraps templates.Get, which is non-nil at runtime
		// (init guarantees defaultID is present).
		slog.Info("edit.start", "slug", in.Slug, "page", in.Page, "template", tmpl.ID, "user", user.Email, "tiers", tiers, "via", "mcp") //nolint:nilaway // see comment.
		// contextcheck: Start intentionally runs detached under its own
		// build-timeout context, same as the web startBuild path.
		s.build.Start(build.Params{ //nolint:contextcheck // see comment.
			Slug:       in.Slug,
			Prompt:     s.build.EditPromptWithHistory(ctx, in.Slug, prompt, in.Page),
			LogKey:     "edit",
			Template:   tmpl,
			Seeds:      s.build.EditSeeds(ctx, in.Slug, prompt),
			UserPrompt: prompt,
			Page:       in.Page,
			Tiers:      tiers,
		})

		st := s.waitForBuild(ctx, in.Slug, runEditWait(in.WaitSeconds))
		res := map[string]any{
			"ok":     st.Status == events.StatusCompleted,
			"slug":   in.Slug,
			"status": st.Status,
			"url":    s.mcpSiteURL(in.Slug),
		}
		if st.Error != "" {
			res["error"] = st.Error
		}
		if !events.IsTerminal(st.Status) {
			res["next"] = "the run is still going — poll list_runs and read the newest transcript with get_run_transcript"
			return mcpJSON(res)
		}

		key, tr, ok := s.newestEditTranscript(ctx, in.Slug, startedAfter)
		if ok {
			res["transcript_key"] = key
			res["files_changed"] = editrec.ChangedPaths(tr)
			res["final_messages"] = tr.FinalMessages
		}
		return mcpJSON(res)
	})
}

func runEditWait(seconds int) time.Duration {
	if seconds <= 0 {
		return runEditDefaultWait
	}
	// Clamp before multiplying: time.Duration(seconds)*time.Second wraps
	// negative for huge inputs, which the post-multiply cap would miss —
	// yielding a deadline in the past and an unrequested fire-and-forget.
	if seconds > int(runEditMaxWait/time.Second) {
		return runEditMaxWait
	}
	return time.Duration(seconds) * time.Second
}

// waitForBuild polls the events tracker until the slug reaches a terminal
// status (completed/failed — linting, retry, and polishing are all still
// in-flight) or the wait budget (or the request context) runs out, returning
// the last state seen so the caller reads status and error from one coherent
// snapshot. Polling rather than subscribing keeps this independent of the
// SSE plumbing's buffer semantics.
func (s *Server) waitForBuild(ctx context.Context, slug string, wait time.Duration) *events.Status {
	deadline := time.Now().Add(wait)
	last := &events.Status{Slug: slug, Status: events.StatusBuilding}
	for {
		st := s.events.Get(slug)
		if st == nil {
			// The entry existed when build.Start returned (events.Start is
			// synchronous), so nil means it was forgotten — the site was
			// deleted mid-run. Nothing left to wait on.
			return last
		}
		last = st
		if events.IsTerminal(st.Status) || time.Now().After(deadline) {
			return st
		}
		select {
		case <-ctx.Done():
			return st
		case <-time.After(runEditPollEvery):
		}
	}
}

// newestEditTranscript returns the most recent "edit" transcript written at
// or after startedAfter — the run this tool call started, unless recording is
// disabled or the write failed (both best-effort by design).
func (s *Server) newestEditTranscript(ctx context.Context, slug string, startedAfter time.Time) (string, editrec.Transcript, bool) {
	rows, err := editrec.List(ctx, s.store, slug)
	if err != nil {
		return "", editrec.Transcript{}, false
	}
	for _, row := range rows {
		if row.LogKey != "edit" || row.Timestamp.Before(startedAfter) {
			continue
		}
		tr, err := editrec.Read(ctx, s.store, row.Key)
		if err != nil {
			return "", editrec.Transcript{}, false
		}
		return row.Key, tr, true
	}
	return "", editrec.Transcript{}, false
}
