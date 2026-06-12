package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// minioStore returns the test store: in-memory by default, S3/Minio when
// AWS_ENDPOINT_URL + S3_BUCKET are set (see internal/storetest). Never nil —
// the `if st == nil { t.Skip }` gates this used to require are gone, so the
// server e2e suite runs deterministically in plain `go test`. Kept as a named
// wrapper so the many call sites read unchanged.
func minioStore(t *testing.T) *store.Store {
	t.Helper()
	return storetest.New(t, 0)
}

func freshSlug(t *testing.T) string {
	t.Helper()
	return storetest.FreshSlug(t, "srvtest")
}

func mustWrite(t *testing.T, ctx context.Context, s *store.Store, slug, path, content, ct string) {
	t.Helper()
	err := s.Write(ctx, slug, path, content, ct, nil)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(t *testing.T, ctx context.Context, s *store.Store, slug, path string) string {
	t.Helper()
	obj, err := s.Read(ctx, slug, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return obj.Content
}

func cleanupSlug(t *testing.T, ctx context.Context, s *store.Store, svc *snapshot.Service, slug string) {
	t.Helper()
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
		snaps, _ := svc.List(ctx, slug)
		for _, sn := range snaps {
			_ = svc.Delete(ctx, slug, sn.Key)
		}
	})
}

const testAdminUser = "admin@test"

// buildServer wires up a Server with all the dependencies the restore + delete
// routes touch. LLM and Sandbox are left nil — neither code path goes near
// them. Also seeds a passkey session for the testAdminUser so the gated
// routes (everything under requireUser) authenticate via the cookie set
// by InjectTestSession.
func buildServer(t *testing.T, st *store.Store, snapSvc *snapshot.Service) http.Handler {
	t.Helper()
	return buildServerWithState(t, st, snapSvc, state.NewMemory())
}

// buildServerWithState is buildServer with an injectable KV state store, for
// tests that need to seed or inspect form-submission data (see data_test.go).
func buildServerWithState(t *testing.T, st *store.Store, snapSvc *snapshot.Service, kv state.Store) http.Handler {
	t.Helper()
	authSvc, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: testAdminUser,
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = authSvc.Close() })
	token, err := authSvc.InjectTestSession(context.Background(), testAdminUser, auth.RoleSuperAdmin)
	if err != nil {
		t.Fatalf("inject test session: %v", err)
	}
	testSessionCookie = &http.Cookie{Name: authSvc.SessionCookieName(), Value: token}
	buildTracker := events.NewTracker()
	t.Cleanup(buildTracker.Close)
	depsTracker := events.NewTracker()
	t.Cleanup(depsTracker.Close)
	e, _ := server.New(server.Deps{
		Store:    st,
		Build:    build.New(st, nil, buildTracker, snapSvc),
		Events:   depsTracker,
		State:    kv,
		Snapshot: snapSvc,
		Auth:     authSvc,
		Domain:   "localhost",
		Port:     "8080",
	})
	return e
}

// TestHistoryRestoreHandler_EndToEnd drives POST /history/:slug/restore through
// the full Echo stack (subdomainMiddleware → requireUser → handler →
// snapshot.Restore) against a real Minio bucket, then asserts both the HTTP
// response and the on-disk state.
func TestHistoryRestoreHandler_EndToEnd(t *testing.T) {
	st := minioStore(t)

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	handler := buildServer(t, st, snapSvc)

	// Seed v1 via the store directly so we don't need a working LLM.
	mustWrite(t, ctx, st, slug, "index.html", "<h1>v1</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "_state/data.json", `{"count":1}`, "application/json")
	snap, err := snapSvc.Create(ctx, slug, snapshot.ReasonBuild)
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Mutate to v2 — different index, extra file, different state.
	mustWrite(t, ctx, st, slug, "index.html", "<h1>v2 different</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "extra.html", "<p>added later</p>", "text/html")
	mustWrite(t, ctx, st, slug, "_state/data.json", `{"count":99}`, "application/json")

	body := url.Values{"key": {snap.Key}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/history/"+slug, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-HTTP-Method-Override", "PUT")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/workspace/"+slug) || !strings.Contains(loc, "flash=") {
		t.Errorf("redirect: got %q, want /workspace/<slug>?flash=...", loc)
	}

	if got := mustRead(t, ctx, st, slug, "index.html"); got != "<h1>v1</h1>" {
		t.Errorf("index post-restore: got %q want %q", got, "<h1>v1</h1>")
	}
	if got := mustRead(t, ctx, st, slug, "_state/data.json"); got != `{"count":1}` {
		t.Errorf("state post-restore: got %q want %q", got, `{"count":1}`)
	}
	if got := mustRead(t, ctx, st, slug, "extra.html"); got != "" {
		t.Errorf("extra.html should have been wiped, got %q", got)
	}

	snaps, err := snapSvc.List(ctx, slug)
	if err != nil {
		t.Fatalf("list post-restore: %v", err)
	}
	foundPreRestore := false
	for _, sn := range snaps {
		if sn.Reason == snapshot.ReasonPreRestore {
			foundPreRestore = true
		}
	}
	if !foundPreRestore {
		t.Errorf("expected a pre-restore snapshot in list, got %+v", snaps)
	}
}

// TestHistoryRestoreHandler_RejectsUnauth confirms requireUser guards the
// restore route. Without a session cookie, the request is bounced to /login
// before any S3 work happens. (Pre-passkey this returned 401; the cutover
// in 725d605 swapped that for a 303 redirect so the user has a way back in.)
func TestHistoryRestoreHandler_RejectsUnauth(t *testing.T) {
	st := minioStore(t)
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)

	req := httptest.NewRequest(http.MethodPost, "/history/some-slug", strings.NewReader("key=anything"))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-HTTP-Method-Override", "PUT")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/login" {
		t.Errorf("redirect location: got %q want %q", loc, "/login")
	}
}

// TestSettingsDeleteHandler_EndToEnd drives POST /settings/:slug/delete, then
// asserts the slug's files and snapshots are gone.
func TestSettingsDeleteHandler_EndToEnd(t *testing.T) {
	st := minioStore(t)

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	// Cleanup guards against a half-deleted state in case of test failure.
	cleanupSlug(t, ctx, st, snapSvc, slug)

	handler := buildServer(t, st, snapSvc)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>doomed</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "about.html", "<h1>also doomed</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "_state/data.json", `{"count":7}`, "application/json")
	_, err := snapSvc.Create(ctx, slug, snapshot.ReasonBuild)
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Wrong confirmation → 400, nothing deleted.
	body := url.Values{"confirm": {"not-the-slug"}}.Encode()
	req := httptest.NewRequest(http.MethodPost, "/apps/"+slug, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-HTTP-Method-Override", "DELETE")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("wrong-confirm status: got %d want 400; body=%q", rec.Code, rec.Body.String())
	}
	files, _ := st.List(ctx, slug)
	if len(files) == 0 {
		t.Fatalf("files were wiped despite wrong confirmation")
	}

	// Correct confirmation → 303, files + snapshots gone.
	body = url.Values{"confirm": {slug}}.Encode()
	req = httptest.NewRequest(http.MethodPost, "/apps/"+slug, strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-HTTP-Method-Override", "DELETE")
	req.AddCookie(testSessionCookie)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("delete status: got %d want 303; body=%q", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/apps") || !strings.Contains(loc, "flash=") {
		t.Errorf("redirect: got %q, want /apps?flash=...", loc)
	}

	files, err = st.List(ctx, slug)
	if err != nil {
		t.Fatalf("list post-delete: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected no files after delete, got %v", files)
	}
	snaps, err := snapSvc.List(ctx, slug)
	if err != nil {
		t.Fatalf("snapshot list post-delete: %v", err)
	}
	if len(snaps) != 0 {
		t.Errorf("expected no snapshots after delete, got %d", len(snaps))
	}
}
