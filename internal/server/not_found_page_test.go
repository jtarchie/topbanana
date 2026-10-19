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

// TestProxyServesSite404Page confirms a missing path on a site with its own
// 404.html gets that page with a 404 status, at any depth, and that a site
// without one (or a reserved path) still gets the platform's generic 404.
func TestProxyServesSite404Page(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)

	withPage := "nf-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, withPage)
	mustWrite(t, ctx, st, withPage, "index.html", "<html><body>home</body></html>", "text/html; charset=utf-8")
	mustWrite(t, ctx, st, withPage, "404.html", "<html><body>passed on</body></html>", "text/html; charset=utf-8")

	without := "nf-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, without)
	mustWrite(t, ctx, st, without, "index.html", "<html><body>home</body></html>", "text/html; charset=utf-8")

	srv := httptest.NewServer(buildServer(t, st, snapSvc))
	t.Cleanup(srv.Close)

	get := func(slug, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		req.Host = slug + ".localhost"
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	for _, path := range []string{"/missing.html", "/deep/nested/missing", "/gone.png"} {
		code, body := get(withPage, path)
		if code != http.StatusNotFound || !strings.Contains(body, "passed on") {
			t.Errorf("%s: got %d %q, want 404 with the site's page", path, code, body)
		}
	}

	if code, body := get(withPage, "/_state/data.json"); code != http.StatusNotFound || strings.Contains(body, "passed on") {
		t.Errorf("reserved path: got %d %q, want the generic 404", code, body)
	}

	if code, body := get(without, "/missing.html"); code != http.StatusNotFound || strings.Contains(body, "passed on") {
		t.Errorf("site without 404.html: got %d %q, want the generic 404", code, body)
	}

	if code, body := get(withPage, "/"); code != http.StatusOK || !strings.Contains(body, "home") {
		t.Errorf("index: got %d %q", code, body)
	}
}

// TestSiteErrorPage confirms visitors of a hosted site get the neutral,
// self-contained site error page (naming the site, linking its home, showing
// the missing path) while the admin host keeps the platform's banana page.
func TestSiteErrorPage(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)

	slug := "se-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, ".topbanana.json", `{"template":"blank","title":"Fixture Chapel"}`, "application/json")
	mustWrite(t, ctx, st, slug, "index.html", "<html><body>home</body></html>", "text/html; charset=utf-8")

	srv := httptest.NewServer(buildServer(t, st, snapSvc))
	t.Cleanup(srv.Close)

	get := func(host, path string) (int, string) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		req.Host = host
		req.Header.Set("Accept", "text/html")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET %s%s: %v", host, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	code, body := get(slug+".localhost", "/teachings.html")
	if code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", code)
	}
	for _, want := range []string{"Page not found", "Go to Fixture Chapel", `href="/"`, "/teachings.html", "<style>"} {
		if !strings.Contains(body, want) {
			t.Errorf("site 404 missing %q", want)
		}
	}
	for _, banned := range []string{"missing a banana", `href="/app.css"`} {
		if strings.Contains(body, banned) {
			t.Errorf("site 404 must not contain %q", banned)
		}
	}

	code, body = get("no-such-site.localhost", "/x")
	if code != http.StatusNotFound || !strings.Contains(body, "Page not found") || strings.Contains(body, "Go to ") {
		t.Errorf("unknown host: got %d, want neutral 404 with no home link; body %q", code, body)
	}

	code, body = get("localhost", "/s/no-such-site/")
	if code != http.StatusNotFound || !strings.Contains(body, "missing a banana") {
		t.Errorf("admin host: got %d, want the banana 404", code)
	}
}
