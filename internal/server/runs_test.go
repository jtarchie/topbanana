package server_test

// The run feed endpoint backs the workspace conversation panel. These tests
// pin its contract: newest-first entries with verdict data, the undo key only
// on the newest run (older restores belong to Version history), machinery
// runs filtered out, and the ownership gate answering 404 to strangers.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// seedRunTranscriptRaw writes a minimal transcript the feed can project.
func seedRunTranscriptRaw(t *testing.T, st *store.Store, slug, logKey, prompt string, at time.Time, files []string, finalMsg string) {
	t.Helper()
	changes := make([]editrec.FileChange, 0, len(files))
	for i, f := range files {
		changes = append(changes, editrec.FileChange{
			Path:         f,
			BeforeSHA256: fmt.Sprintf("b%d", i),
			AfterSHA256:  fmt.Sprintf("a%d", i),
		})
	}
	tr := editrec.Transcript{
		Slug:        slug,
		LogKey:      logKey,
		StartedAt:   at,
		FinishedAt:  at.Add(30 * time.Second),
		UserPrompt:  prompt,
		FinalStatus: "completed",
		FileChanges: changes,
	}
	if finalMsg != "" {
		tr.FinalMessages = []string{finalMsg}
	}
	body, err := json.Marshal(tr)
	if err != nil {
		t.Fatalf("marshal transcript: %v", err)
	}
	err = st.WriteRaw(context.Background(), editrec.Key(slug, at, logKey), string(body), "application/json", nil)
	if err != nil {
		t.Fatalf("seed transcript: %v", err)
	}
}

func getRunsFeed(t *testing.T, rig *privateTestRig, slug string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/runs/"+slug, nil)
	// The rig's domain is localhost; any other host is treated as a hosted
	// site by the subdomain dispatcher and never reaches the admin routes.
	req.Host = "localhost"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	return rec
}

func TestRunsFeed_VerdictsUndoKeyAndOrdering(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)
	snapSvc := snapshot.New(st, 0)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "runsfeed-owner@example.com"

	// Seed before server.New so the registry's initial index rebuild records
	// the ownership — same ordering contract as buildMCPTestServer.
	mustWrite(t, ctx, st, slug, "index.html", "<html><head></head><body>hi</body></html>", "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner})
	rig := newPrivateRig(t, st, snapSvc)

	now := time.Now().UTC()
	seedRunTranscriptRaw(t, st, slug, "edit", "make the line white", now.Add(-10*time.Minute), nil, "the text is already white")
	seedRunTranscriptRaw(t, st, slug, "relint", "machinery", now.Add(-5*time.Minute), []string{"index.html"}, "")
	seedRunTranscriptRaw(t, st, slug, "edit", "remove the word occasionally", now.Add(-1*time.Minute), []string{"index.html"}, "done")

	// The pre-run snapshot the newest run's Undo restores.
	_, err := snapSvc.Create(ctx, slug, snapshot.ReasonEdit)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}

	res := getRunsFeed(t, rig, slug, rig.session(t, owner, auth.RoleAdmin))
	if res.Code != http.StatusOK {
		t.Fatalf("GET /runs = %d, want 200: %s", res.Code, res.Body.String())
	}
	var body struct {
		Runs []struct {
			Prompt       string   `json:"prompt"`
			Status       string   `json:"status"`
			Files        []string `json:"files"`
			Key          string   `json:"key"`
			FinalMessage string   `json:"final_message"`
			UndoKey      string   `json:"undo_key"`
		} `json:"runs"`
	}
	err = json.Unmarshal(res.Body.Bytes(), &body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Runs) != 2 {
		t.Fatalf("runs = %d, want 2 (machinery relint filtered out)", len(body.Runs))
	}
	newest, older := body.Runs[0], body.Runs[1]

	if newest.Prompt != "remove the word occasionally" {
		t.Fatalf("newest first, got %q", newest.Prompt)
	}
	if len(newest.Files) != 1 || newest.Files[0] != "index.html" {
		t.Fatalf("newest files = %v", newest.Files)
	}
	if newest.UndoKey == "" {
		t.Fatal("newest run must carry the undo snapshot key")
	}
	if newest.Key == "" {
		t.Fatal("entries must carry the transcript key for the details link")
	}

	if older.UndoKey != "" {
		t.Fatalf("older run carries undo_key %q — one-click undo is newest-only", older.UndoKey)
	}
	if len(older.Files) != 0 {
		t.Fatalf("no-op run files = %v, want empty", older.Files)
	}
	if older.FinalMessage != "the text is already white" {
		t.Fatalf("no-op run must carry the agent's explanation, got %q", older.FinalMessage)
	}
}

func TestRunsFeed_NonOwnerGets404(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)
	ctx := context.Background()
	slug := freshSlug(t)

	mustWrite(t, ctx, st, slug, "index.html", "<html></html>", "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: "someone-else@example.com"})
	rig := newPrivateRig(t, st, snapshot.New(st, 0))

	res := getRunsFeed(t, rig, slug, rig.session(t, "stranger@example.com", auth.RoleAdmin))
	if res.Code != http.StatusNotFound {
		t.Fatalf("non-owner GET /runs = %d, want 404", res.Code)
	}
}
