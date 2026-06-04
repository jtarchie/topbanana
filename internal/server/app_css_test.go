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

// TestAppCSSHandler_ServesEmbeddedSheet confirms the admin host serves the
// precompiled, self-contained stylesheet as text/css with no CDN references.
func TestAppCSSHandler_ServesEmbeddedSheet(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/app.css", nil)
	req.Host = "localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /app.css: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) == 0 {
		t.Fatal("empty /app.css body")
	}
	if strings.Contains(string(body), "cdn.jsdelivr.net") {
		t.Error("/app.css must not reference the CDN")
	}
}

// TestVisualEditorLoadsSiteAppCSSIntoCanvas confirms the GrapesJS editor wires
// the site's compiled /app.css and the page's data-theme into the canvas — so
// the editor renders components as published, not unstyled.
func TestVisualEditorLoadsSiteAppCSSIntoCanvas(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := "vised-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html data-theme="synthwave"><head><title>v</title><link rel="stylesheet" href="/app.css"></head><body><h1>hi</h1></body></html>`,
		"text/html; charset=utf-8")

	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/edit/"+slug+"/visual?page=index.html", nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET editor: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	got := string(body)

	// canvasCSS must point at the site's own /app.css, and the theme must flow
	// through so the canvas <html> can be set.
	if !strings.Contains(got, slug+".localhost") || !strings.Contains(got, "/app.css") {
		t.Errorf("editor did not wire the site /app.css URL into the canvas; body:\n%s", got)
	}
	if !strings.Contains(got, `pageTheme = "synthwave"`) {
		t.Errorf("editor did not pass the page data-theme; want pageTheme = \"synthwave\"")
	}
}

// TestProxyServesSiteAppCSS confirms a per-site app.css in S3 is served on the
// site subdomain as text/css and is not blocked by the reserved-prefix guard.
func TestProxyServesSiteAppCSS(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := "appcss-" + freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "app.css", ".btn{color:red}", "text/css; charset=utf-8")

	handler := buildServer(t, st, snapSvc)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/app.css", nil)
	req.Host = slug + ".localhost"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET site /app.css: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), ".btn") {
		t.Errorf("unexpected body: %q", body)
	}
}
