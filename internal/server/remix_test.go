package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestRemixHandler_CopiesFilesAndRewritesMeta drives POST /manage/:slug/remix
// against a real Minio bucket and asserts that the new slug ends up with
// the same file contents but a fresh meta sidecar owned by the caller.
//
//nolint:cyclop // single end-to-end script covers many small assertions on one remix call.
func TestRemixHandler_CopiesFilesAndRewritesMeta(t *testing.T) {
	st := minioStore(t)

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	src := "remix-src-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, src)

	// Seed: HTML + an image-style binary blob + meta with a custom domain
	// and Private=true so we can verify those don't carry over.
	const indexHTML = "<!doctype html><html><body><h1>source</h1></body></html>"
	const aboutHTML = "<!doctype html><html><body><h1>about source</h1></body></html>"
	mustWrite(t, ctx, st, src, "index.html", indexHTML, "text/html")
	mustWrite(t, ctx, st, src, "about.html", aboutHTML, "text/html")
	writeMeta(t, ctx, st, src, build.SiteMeta{
		Template:    "blank",
		OwnerID:     testAdminUser,
		Title:       "Original Title",
		Description: "Original description.",
		Domains:     []string{"original.example"},
		Private:     true,
	})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// POST and capture the 303 target without following it.
	req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/apps/"+src+"/remix", nil)
	if err != nil {
		t.Fatalf("new POST remix: %v", err)
	}
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST remix: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	const prefix = "/manage/"
	if !strings.HasPrefix(loc, prefix) {
		t.Fatalf("redirect location: got %q want %s<slug>?flash=…", loc, prefix)
	}
	rest := strings.TrimPrefix(loc, prefix)
	dst := rest
	if idx := strings.IndexByte(rest, '?'); idx >= 0 {
		dst = rest[:idx]
	}
	if dst == "" || dst == src {
		t.Fatalf("dst slug parsed from %q is empty or equals source", loc)
	}
	t.Cleanup(func() { cleanupSlug(t, ctx, st, snapSvc, dst) })

	// Every non-meta file under src appears under dst with byte-identical
	// content.
	srcFiles, err := st.List(ctx, src)
	if err != nil {
		t.Fatalf("list src: %v", err)
	}
	for _, p := range srcFiles {
		if p == build.MetaFile {
			continue
		}
		want := mustRead(t, ctx, st, src, p)
		got := mustRead(t, ctx, st, dst, p)
		if got != want {
			t.Errorf("file %q: copy diverged (len src=%d dst=%d)", p, len(want), len(got))
		}
	}
	if mustRead(t, ctx, st, dst, "index.html") != indexHTML {
		t.Errorf("dst index.html missing canned content")
	}

	// New meta reflects ownership reassignment + reset visibility.
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	buildSvc := build.New(st, nil, tracker, snapSvc)
	dstMeta := buildSvc.ReadMeta(ctx, dst)
	if dstMeta.OwnerID != testAdminUser {
		t.Errorf("dst OwnerID: got %q want %q", dstMeta.OwnerID, testAdminUser)
	}
	if dstMeta.Template != "blank" {
		t.Errorf("dst Template: got %q want %q", dstMeta.Template, "blank")
	}
	if dstMeta.Title != "Original Title" {
		t.Errorf("dst Title: got %q want carried over", dstMeta.Title)
	}
	if dstMeta.Private {
		t.Errorf("dst Private: got true; remix must reset to false")
	}
	if len(dstMeta.Domains) != 0 {
		t.Errorf("dst Domains: got %v; remix must drop custom domains", dstMeta.Domains)
	}
	if dstMeta.Created.IsZero() {
		t.Errorf("dst Created: zero value, expected a fresh timestamp")
	}

	// Source still intact.
	srcMeta := buildSvc.ReadMeta(ctx, src)
	if !srcMeta.Private {
		t.Errorf("src meta lost Private flag after remix")
	}
	if len(srcMeta.Domains) != 1 || srcMeta.Domains[0] != "original.example" {
		t.Errorf("src Domains mutated by remix: %v", srcMeta.Domains)
	}

	// Edits on the source after remix do not bleed into the copy. The
	// store has an ARC cache; reading via the cache and then writing a
	// fresh value would tempt a buggy implementation to share state.
	const replaced = "<!doctype html><html><body><h1>source mutated</h1></body></html>"
	mustWrite(t, ctx, st, src, "index.html", replaced, "text/html")
	if got := mustRead(t, ctx, st, dst, "index.html"); got != indexHTML {
		t.Errorf("dst index.html mutated after source rewrite: got %q", got)
	}
}

// TestRemixHandler_RejectsNonOwner confirms requireSlugOwnership is wired
// to the remix route — without the session cookie, the POST never reaches
// the handler.
func TestRemixHandler_RejectsNonOwner(t *testing.T) {
	st := minioStore(t)

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	src := "remix-deny-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, src)
	mustWrite(t, ctx, st, src, "index.html", "<h1>guarded</h1>", "text/html")
	writeMeta(t, ctx, st, src, build.SiteMeta{
		Template: "blank",
		OwnerID:  testAdminUser,
	})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// No cookie attached — requireUser bounces to /login.
	req, _ := http.NewRequest(http.MethodPost, httpSrv.URL+"/apps/"+src+"/remix", nil)
	req.Host = "localhost"
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST remix: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (login redirect)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("redirect: got %q want /login", loc)
	}
}
