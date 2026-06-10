package server_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestThemePreviewListener_AlwaysInjected pins down the regression where the
// workspace Themes panel's live Preview stopped updating the iframe.
//
// The workspace fires postMessage({type:'topbanana:settheme', theme}) at the
// iframe loading slug.<domain>; the iframe needs the matching listener
// spliced into its HTML to act on it. Before this test landed,
// injectEditToolbar short-circuited the whole splice when canEdit returned
// false — and canEdit always returns false on subdomain requests, because
// the passkey session cookie is scoped to the admin host. Net effect: the
// listener never reached the iframe and Preview was a silent no-op.
//
// The fix injects the listener on every HTML response (it's a no-op without
// a postMessage opener and the theme value is allowlist-guarded) and keeps
// only the visible toolbar UI gated on canEdit.
func TestThemePreviewListener_AlwaysInjected(t *testing.T) {
	st := minioStore(t)

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Seed the site before building the server so initialRebuildDomainIndex
	// picks the slug up — subdomainMiddleware refuses to dispatch unknown
	// slugs, and we want the proxy to reach injectEditToolbar.
	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html><head><title>x</title></head><body><h1>x</h1></body></html>`,
		"text/html; charset=utf-8")

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	get := func(t *testing.T, cookie *http.Cookie) string {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/", nil)
		if err != nil {
			t.Fatalf("new GET site: %v", err)
		}
		req.Host = slug + ".localhost"
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET site: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("status: got %d, want 200; body=%q", resp.StatusCode, string(body))
		}
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	// Cookie-less request mirrors how a browser loads the workspace iframe
	// (subdomain → no admin-host cookie). The listener must be present so
	// Preview works; the toolbar UI must not, since anonymous visitors
	// shouldn't see edit chrome.
	anonBody := get(t, nil)
	if !strings.Contains(anonBody, "topbanana:settheme") {
		t.Errorf("cookie-less response missing theme-preview listener; Preview will not work.\nbody=%q", trim(anonBody, 400))
	}
	if strings.Contains(anonBody, `id="_tb"`) {
		t.Errorf("cookie-less response leaks edit toolbar UI to anonymous visitors")
	}

	// Authed request — admin sees both the listener and the visible toolbar.
	authedBody := get(t, testSessionCookie)
	if !strings.Contains(authedBody, "topbanana:settheme") {
		t.Errorf("authed response missing theme-preview listener")
	}
	if !strings.Contains(authedBody, `id="_tb"`) {
		t.Errorf("authed response missing edit toolbar UI")
	}
}
