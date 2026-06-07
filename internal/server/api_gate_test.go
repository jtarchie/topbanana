package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
)

// buildServerWithSandbox is buildServer plus a real sandbox.Manager so the
// /api/* dispatch path can actually run a handler instead of bailing at the
// `s.sandbox == nil` short-circuit. apiHandler is only meaningful to test
// with a sandbox in place.
func buildServerWithSandbox(t *testing.T, st *store.Store, snapSvc *snapshot.Service) http.Handler {
	t.Helper()
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	buildSvc := build.New(st, nil, tracker, snapSvc)
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
	_, err = authSvc.InjectTestSession(context.Background(), testAdminUser, auth.RoleSuperAdmin)
	if err != nil {
		t.Fatalf("inject test session: %v", err)
	}
	e, _ := server.New(server.Deps{
		Store:    st,
		Build:    buildSvc,
		Events:   tracker,
		Sandbox:  sandbox.New(sandbox.Config{}),
		State:    state.NewMemory(),
		Snapshot: snapSvc,
		Auth:     authSvc,
		Domain:   "localhost",
		Port:     "8080",
	})
	return e
}

// TestAPIHandler_EnablesFunctionsOverrideHonoredOnEmptyTemplate guards the
// gate-skip fix in apiHandler: a site whose meta.Template is empty but whose
// per-site EnablesFunctions override is true MUST be able to serve /api/*.
//
// Before the fix, apiHandler returned 404 the moment meta.Template == "",
// which made the settings-page "Accept visitor input" toggle a no-op for
// any site that landed with no template id recorded. The smoke we caught
// in production: fast-flame-71.apps.topbanana.dev had EnablesFunctions on
// and functions/submit.js in the store, yet POST /api/submit returned 404
// in ~150µs (sandbox never invoked).
func TestAPIHandler_EnablesFunctionsOverrideHonoredOnEmptyTemplate(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := "apigate-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Minimal handler that proves it actually executed: returns 200 with
	// a recognizable body. Uses the same CommonJS shape the email-capture
	// skeleton ships.
	const submitJS = `module.exports = function (request) {
		return response.json({ ok: true, email: request.form.email || "" });
	};`
	mustWrite(t, ctx, st, slug, "functions/submit.js", submitJS, "text/javascript")
	// Empty Template, EnablesFunctions override on — the exact shape the
	// production app got stuck in.
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		OwnerID:          testAdminUser,
		EnablesFunctions: true,
	})

	handler := buildServerWithSandbox(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	body := strings.NewReader("email=test%40example.com")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/submit", body)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	// Hit the subdomain dispatch path the way a browser would, with a
	// matching Origin so checkAPIOrigin lets the POST through.
	req.Host = slug + ".localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d want 200 — empty-Template site with EnablesFunctions=true must serve /api/*", resp.StatusCode)
	}
}

// TestAPIHandler_EmptyTemplateWithoutOverrideStays404 is the inverse guard:
// a legacy brochure site (empty meta, EnablesFunctions false) must keep
// returning 404. This is the regression the dropped `meta.Template == ""`
// gate previously enforced; EffectiveTemplate now carries it via the blank
// fallback whose EnablesFunctions is false.
func TestAPIHandler_EmptyTemplateWithoutOverrideStays404(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := "apigate-noop-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>brochure</h1>", "text/html")
	// No meta at all — simulates a pre-templates legacy site. The api gate
	// must still 404 because EffectiveTemplate falls back to blank, which
	// has EnablesFunctions=false.

	handler := buildServerWithSandbox(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/anything", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = slug + ".localhost"

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d want 404 — brochure site with no override must stay locked out of /api/*", resp.StatusCode)
	}
}
