package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/buildabear/internal/auth"
	"github.com/jtarchie/buildabear/internal/build"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/server"
	"github.com/jtarchie/buildabear/internal/snapshot"
	"github.com/jtarchie/buildabear/internal/state"
	"github.com/jtarchie/buildabear/internal/store"
)

// minioStore mirrors the helper in internal/snapshot/snapshot_test.go: returns
// a Store backed by the dev Minio (or any S3-compatible backend exposed via
// AWS_ENDPOINT_URL + S3_BUCKET), or nil so the caller can t.Skip when env
// isn't set.
func minioStore(t *testing.T) *store.Store {
	t.Helper()
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		return nil
	}
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	s, err := store.New(client, bucket, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	err = s.EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return s
}

func freshSlug(t *testing.T) string {
	t.Helper()
	return "srvtest-" + strconv.FormatInt(time.Now().UnixNano(), 36)
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
	authSvc, err := auth.New(auth.Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: testAdminUser,
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	token, err := authSvc.InjectTestSession(context.Background(), testAdminUser, auth.RoleSuperAdmin)
	if err != nil {
		t.Fatalf("inject test session: %v", err)
	}
	testSessionCookie = &http.Cookie{Name: auth.SessionCookieName, Value: token}
	e, _ := server.New(server.Deps{
		Store:    st,
		Build:    build.New(st, nil, events.NewTracker(), snapSvc),
		Events:   events.NewTracker(),
		State:    state.NewMemory(),
		Snapshot: snapSvc,
		Auth:     authSvc,
		Domain:   "localhost",
		Port:     "8080",
	})
	return e
}

// TestHistoryRestoreHandler_EndToEnd drives POST /history/:slug/restore through
// the full Echo stack (subdomainMiddleware → requireAdmin → handler →
// snapshot.Restore) against a real Minio bucket, then asserts both the HTTP
// response and the on-disk state.
func TestHistoryRestoreHandler_EndToEnd(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

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
	req := httptest.NewRequest(http.MethodPost, "/history/"+slug+"/restore", strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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

// TestHistoryRestoreHandler_RejectsUnauth confirms requireAdmin guards the
// restore route. Without credentials, no S3 work happens.
func TestHistoryRestoreHandler_RejectsUnauth(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)

	req := httptest.NewRequest(http.MethodPost, "/history/some-slug/restore", strings.NewReader("key=anything"))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d want 401", rec.Code)
	}
}

// TestSettingsDeleteHandler_EndToEnd drives POST /settings/:slug/delete, then
// asserts the slug's files and snapshots are gone.
func TestSettingsDeleteHandler_EndToEnd(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

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
	req := httptest.NewRequest(http.MethodPost, "/settings/"+slug+"/delete", strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	req = httptest.NewRequest(http.MethodPost, "/settings/"+slug+"/delete", strings.NewReader(body))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
