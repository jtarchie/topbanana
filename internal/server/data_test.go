package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
)

// seedSubmissions writes a fresh KV snapshot for slug: three object-valued
// submission rows (the shape the manage table renders) plus a scalar counter
// that must stay untouchable through the delete endpoint.
func seedSubmissions(t *testing.T, ctx context.Context, kv state.Store, slug string) {
	t.Helper()
	err := kv.Save(ctx, slug, &state.Snapshot{Data: map[string]any{
		"submission:0001": map[string]any{"name": "Al", "ts": float64(1700000000000)},
		"submission:0002": map[string]any{"name": "Bo", "email": "bo@x.com"},
		"entry:0001":      map[string]any{"msg": "hi"},
		"submission_seq":  float64(2),
	}})
	if err != nil {
		t.Fatalf("seed state: %v", err)
	}
}

// loadStateData reloads the slug's KV blob so assertions see what actually
// persisted, not the in-memory map the handler mutated.
func loadStateData(t *testing.T, ctx context.Context, kv state.Store, slug string) map[string]any {
	t.Helper()
	snap, err := kv.Load(ctx, slug)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	return snap.Data
}

// TestDeleteSubmissionHandler exercises DELETE /data/:slug end-to-end: the
// validation rejections (missing key, confirm mismatch, scalar key, unknown
// key) leave the blob untouched, then the happy path removes exactly one row.
func TestDeleteSubmissionHandler(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "subdel-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>home</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	kv := state.NewMemory()
	seedSubmissions(t, ctx, kv, slug)

	handler := buildServerWithState(t, st, snapSvc, kv)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// Missing key: 400.
	resp := postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"confirm": {"submission:0001"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("missing key: status %d want 400", resp.StatusCode)
	}

	// Confirm mismatch: 400, row survives.
	resp = postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission:0001"},
		"confirm": {"submission:0002"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("confirm mismatch: status %d want 400", resp.StatusCode)
	}
	if loadStateData(t, ctx, kv, slug)["submission:0001"] == nil {
		t.Errorf("submission:0001 removed despite confirm mismatch")
	}

	// Scalar key: not a submission, 400, counter survives.
	resp = postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission_seq"},
		"confirm": {"submission_seq"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("scalar key: status %d want 400", resp.StatusCode)
	}
	if loadStateData(t, ctx, kv, slug)["submission_seq"] == nil {
		t.Errorf("submission_seq counter deleted through the submission endpoint")
	}

	// Unknown key: 404.
	resp = postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission:9999"},
		"confirm": {"submission:9999"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown key: status %d want 404", resp.StatusCode)
	}

	// Happy path: one row gone, siblings and counter intact, redirect carries
	// the flash back to the manage page.
	resp = postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission:0001"},
		"confirm": {"submission:0001"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("happy path: status %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/manage/"+slug) || !strings.Contains(loc, "flash=Deleted") {
		t.Errorf("redirect: got %q want /manage/%s…flash=Deleted…", loc, slug)
	}
	data := loadStateData(t, ctx, kv, slug)
	if data["submission:0001"] != nil {
		t.Errorf("submission:0001 still present after delete")
	}
	for _, key := range []string{"submission:0002", "entry:0001", "submission_seq"} {
		if data[key] == nil {
			t.Errorf("%s missing after deleting an unrelated row", key)
		}
	}
}

// TestDeleteSubmissionRejectsUnauthenticated sanity-checks that the route sits
// behind requireUser — without the session cookie the request never reaches
// the handler (login redirect), mirroring TestFileOpsRejectsNonOwner.
func TestDeleteSubmissionRejectsUnauthenticated(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "subguard-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>guarded</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	kv := state.NewMemory()
	seedSubmissions(t, ctx, kv, slug)

	handler := buildServerWithState(t, st, snapSvc, kv)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	form := url.Values{
		"key":     {"submission:0001"},
		"confirm": {"submission:0001"},
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/data/"+slug, strings.NewReader(form.Encode()))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-HTTP-Method-Override", http.MethodDelete)
	noCookie := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noCookie.Do(req)
	if err != nil {
		t.Fatalf("DELETE /data/%s: %v", slug, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/login" {
		t.Errorf("without cookie: status %d loc %q want 303 /login",
			resp.StatusCode, resp.Header.Get("Location"))
	}
	if loadStateData(t, ctx, kv, slug)["submission:0001"] == nil {
		t.Errorf("submission:0001 removed even though POST was unauthenticated")
	}
}

// conflictingState forces ErrConflict on the first N Saves — the seam for
// asserting deleteSubmissionKey's CAS retry loop (concurrent visitor
// submissions land between the handler's Load and Save).
type conflictingState struct {
	state.Store
	conflictsLeft int
	saves         int
}

func (c *conflictingState) Save(ctx context.Context, slug string, snap *state.Snapshot) error {
	c.saves++
	if c.conflictsLeft > 0 {
		c.conflictsLeft--
		return state.ErrConflict
	}
	// Deliberately unwrapped: the stub must hand the retry loop exactly the
	// error a production store would.
	return c.Store.Save(ctx, slug, snap) //nolint:wrapcheck
}

// TestDeleteSubmissionRetriesThroughConflicts drives the delete through two
// forced CAS conflicts and asserts it still lands, then exhausts the retry
// budget and expects 503 with the row intact.
func TestDeleteSubmissionRetriesThroughConflicts(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "subcas-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>home</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	kv := &conflictingState{Store: state.NewMemory()}
	seedSubmissions(t, ctx, kv, slug)
	kv.conflictsLeft = 2

	handler := buildServerWithState(t, st, snapSvc, kv)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	resp := postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission:0001"},
		"confirm": {"submission:0001"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete with conflicts: status %d want 303", resp.StatusCode)
	}
	if loadStateData(t, ctx, kv, slug)["submission:0001"] != nil {
		t.Errorf("submission:0001 still present after retried delete")
	}

	// Exhaustion: every Save conflicts → 503, and the row survives.
	kv.conflictsLeft = 100
	resp = postFileOps(t, srv.URL, "/data/"+slug, url.Values{
		"_method": {"DELETE"},
		"key":     {"submission:0002"},
		"confirm": {"submission:0002"},
	})
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("exhausted retries: status %d want 503", resp.StatusCode)
	}
	kv.conflictsLeft = 0
	if loadStateData(t, ctx, kv, slug)["submission:0002"] == nil {
		t.Errorf("submission:0002 deleted despite exhausted CAS retries")
	}
}
