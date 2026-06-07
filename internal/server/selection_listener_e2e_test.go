package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestSelectionListener_AlwaysInjected pins the iframe-selection bridge to
// every HTML response on the platform domain. The listener postMessages the
// user's drag-selected text to the workspace parent so the agent prompt can
// be scoped to that excerpt; before this test landed, the listener lived
// inside the canEdit-gated edit toolbar and never reached the iframe, since
// the passkey session cookie is host-scoped to the admin domain. The custom
// domain branch must stay untouched so CDN cache safety holds.
func TestSelectionListener_AlwaysInjected(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	subdomainSlug := freshSlug(t)
	customSlug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, subdomainSlug)
	cleanupSlug(t, ctx, st, snapSvc, customSlug)

	mustWrite(t, ctx, st, subdomainSlug, "index.html",
		`<!DOCTYPE html><html><head><title>x</title></head><body><h1>x</h1></body></html>`,
		"text/html; charset=utf-8")
	mustWrite(t, ctx, st, customSlug, "index.html",
		`<!DOCTYPE html><html><head><title>x</title></head><body><h1>x</h1></body></html>`,
		"text/html; charset=utf-8")

	// Register a custom domain on customSlug. server.New runs the initial
	// domain-index rebuild during construction, so the meta has to land
	// before buildServer below.
	customHost := customSlug + ".example.test"
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	buildSvc := build.New(st, nil, tracker, snapSvc)
	err := buildSvc.WriteMeta(ctx, customSlug, build.SiteMeta{
		Created: time.Now().UTC(),
		Domains: []string{customHost},
	})
	if err != nil {
		t.Fatalf("write meta: %v", err)
	}

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	get := func(t *testing.T, host string, cookie *http.Cookie) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
		if err != nil {
			t.Fatalf("new GET site: %v", err)
		}
		req.Host = host
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", host, err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status: got %d, want 200; body=%q", resp.StatusCode, string(body))
		}
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	// Cookie-less subdomain request mirrors how the workspace iframe loads:
	// the admin-host cookie isn't scoped to the subdomain, so canEdit is
	// false. The selection listener must still be present so drag-select
	// reaches the parent; the visible toolbar UI must not, since anonymous
	// visitors shouldn't see edit chrome.
	anonBody := get(t, subdomainSlug+".localhost", nil)
	if !strings.Contains(anonBody, "topbanana:selection") {
		t.Errorf("cookie-less response missing selection bridge; agent loses iframe context.\nbody=%q", trim(anonBody, 400))
	}
	if !strings.Contains(anonBody, "window.parent === window") {
		t.Errorf("cookie-less response missing parent-window guard; direct visitors would postMessage to themselves")
	}
	if strings.Contains(anonBody, `id="_tb"`) {
		t.Errorf("cookie-less response leaks edit toolbar UI to anonymous visitors")
	}

	// Authed subdomain request — admin sees both the bridge and the toolbar.
	authedBody := get(t, subdomainSlug+".localhost", testSessionCookie)
	if !strings.Contains(authedBody, "topbanana:selection") {
		t.Errorf("authed response missing selection bridge")
	}
	if !strings.Contains(authedBody, `id="_tb"`) {
		t.Errorf("authed response missing edit toolbar UI")
	}

	// Custom-domain request — neither listener nor toolbar is injected, so
	// the CDN can cache the response publicly without leaking admin chrome.
	customBody := get(t, customHost, nil)
	if strings.Contains(customBody, "topbanana:selection") {
		t.Errorf("custom-domain response includes selection bridge; CDN would cache admin chrome.\nbody=%q", trim(customBody, 400))
	}
	if strings.Contains(customBody, "topbanana:settheme") {
		t.Errorf("custom-domain response includes theme listener; CDN would cache admin chrome")
	}
	if strings.Contains(customBody, `id="_tb"`) {
		t.Errorf("custom-domain response includes edit toolbar UI")
	}
}
