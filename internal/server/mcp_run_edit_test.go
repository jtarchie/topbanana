package server_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

// editStubRunner stands in for the real agent on the run_edit path: it emits
// the same event shapes the production runner does (tool start/done around a
// store write, then the final-text event), so editrec records a FileChange
// and a final message exactly as a live run would.
type editStubRunner struct{}

// stubEditedPage is lint-clean for the default template so the build service
// finishes in one pass without a lint retry.
const stubEditedPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Stub Edited Page</title>
<meta name="description" content="A page rewritten by the run_edit stub runner during tests.">
<link rel="stylesheet" href="/app.css">
</head>
<body><h1>Edited by stub</h1></body>
</html>
`

func (editStubRunner) Run(ctx context.Context, s *store.Store, req build.RunRequest, emit func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	emit(events.Event{Type: events.TypeTool, Tool: "edit_file", Phase: events.PhaseStart, Path: "index.html"})
	err := s.Write(ctx, req.Slug, "index.html", stubEditedPage, "text/html; charset=utf-8", nil)
	if err != nil {
		emit(events.Event{Type: events.TypeTool, Tool: "edit_file", Phase: events.PhaseError, Path: "index.html", Message: err.Error()})
		return agent.Usage{}, err //nolint:wrapcheck // stub passthrough
	}
	emit(events.Event{Type: events.TypeTool, Tool: "edit_file", Phase: events.PhaseDone, Path: "index.html"})
	emit(events.Event{Type: events.TypeAgentText, Message: "done"})
	return agent.Usage{}, nil
}

func (editStubRunner) Describe(context.Context, *store.Store, string, string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

// TestMCP_RunEdit_EndToEnd drives run_edit through the full stack: bearer
// auth, ownership, the build service (stub runner), transcript recording, and
// the tool's result assembly (status, files_changed, final_messages).
func TestMCP_RunEdit_EndToEnd(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	mustWrite(t, ctx, st, slug, "index.html", "<html><head></head><body><h1>Hello</h1></body></html>", "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner})

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	authBlobs := blob.NewMemory()
	authSvc, err := auth.New(auth.Config{
		Blobs:           authBlobs,
		Domain:          "localhost",
		SuperAdminEmail: "super@example.com",
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = authSvc.Close() })

	buildSvc := build.NewWithConfig(build.Config{
		Store:      st,
		Runner:     editStubRunner{},
		Events:     tracker,
		Snapshot:   snapshot.New(st, 0),
		RecordEdit: true,
	})

	e, _ := server.New(server.Deps{
		Store:     st,
		Build:     buildSvc,
		Events:    tracker,
		Sandbox:   sandbox.New(sandbox.Config{}),
		State:     state.NewMemory(),
		Snapshot:  snapshot.New(st, 0),
		Auth:      authSvc,
		Blobs:     authBlobs,
		Domain:    "localhost",
		Port:      "8080",
		MCPSecret: mcpTestSecret,
	})
	srv := httptest.NewServer(e)
	t.Cleanup(srv.Close)

	session := connectMCP(t, srv, owner)
	seedUser(t, authBlobs, owner)

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "run_edit", Arguments: map[string]any{
			"slug":   slug,
			"prompt": "Change the heading to say Edited by stub.",
		},
	})
	if err != nil {
		t.Fatalf("run_edit: %v", err)
	}
	if res.IsError {
		t.Fatalf("run_edit errored: %s", toolText(res))
	}

	var out struct {
		OK            bool     `json:"ok"`
		Status        string   `json:"status"`
		TranscriptKey string   `json:"transcript_key"`
		FilesChanged  []string `json:"files_changed"`
		FinalMessages []string `json:"final_messages"`
	}
	err = json.Unmarshal([]byte(toolText(res)), &out)
	if err != nil {
		t.Fatalf("decode result %q: %v", toolText(res), err)
	}
	if !out.OK || out.Status != events.StatusCompleted {
		t.Fatalf("run_edit did not complete: %+v", out)
	}
	if len(out.FilesChanged) != 1 || out.FilesChanged[0] != "index.html" {
		t.Fatalf("files_changed = %v, want [index.html]", out.FilesChanged)
	}
	if len(out.FinalMessages) == 0 || out.FinalMessages[0] != "done" {
		t.Fatalf("final_messages = %v, want [done]", out.FinalMessages)
	}
	if !strings.HasPrefix(out.TranscriptKey, "_edits/"+slug+"/") {
		t.Fatalf("transcript_key = %q, want under _edits/%s/", out.TranscriptKey, slug)
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(obj.Content, "Edited by stub") {
		t.Fatalf("edit not persisted: %s", obj.Content)
	}
}

// TestMCP_RunEdit_NonOwnerSeesNotFound keeps the slug-existence guarantee:
// a caller without access must get "not found", never a build kicked off.
func TestMCP_RunEdit_NonOwnerSeesNotFound(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)
	const owner = "owner@example.com"

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	session := connectMCP(t, srv, "stranger@example.com")
	seedUser(t, authBlobs, owner)
	seedUser(t, authBlobs, "stranger@example.com")

	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{
		Name: "run_edit", Arguments: map[string]any{
			"slug":   slug,
			"prompt": "Change the heading.",
		},
	})
	if err != nil {
		t.Fatalf("run_edit: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected not-found error, got: %s", toolText(res))
	}
	if !strings.Contains(strings.ToLower(toolText(res)), "not found") {
		t.Fatalf("error should read as not found: %s", toolText(res))
	}
}
