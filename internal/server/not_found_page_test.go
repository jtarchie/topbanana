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
