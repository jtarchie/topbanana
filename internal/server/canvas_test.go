package server_test

// The canvas (v2 workspace) rests on one contract: a page served with
// ?tb_edit=1 to an authorized caller carries element addresses and the
// selection script, and carries neither for anyone else. These tests pin
// that, plus the canvas page render itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/storetest"
)

const canvasTestPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>t</title></head>
<body>
<h1>Hello</h1>
<p>Same text</p>
<p>Same text</p>
</body>
</html>`

func canvasRigGet(t *testing.T, rig *privateTestRig, path string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "localhost"
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	rig.handler.ServeHTTP(rec, req)
	return rec
}

func TestProxyEditMode_StampsAddressesForOwnerOnly(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "canvas-owner@example.com"

	mustWrite(t, ctx, st, slug, "index.html", canvasTestPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner})
	rig := newPrivateRig(t, st, snapshot.New(st, 0))
	ownerCookie := rig.session(t, owner, auth.RoleAdmin)

	res := canvasRigGet(t, rig, "/s/"+slug+"/index.html?tb_edit=1", ownerCookie)
	if res.Code != http.StatusOK {
		t.Fatalf("edit mode GET = %d, want 200", res.Code)
	}
	body := res.Body.String()
	for _, want := range []string{
		`<html data-tb-el="0"`,
		`data-tb-el="4"`, // body — html, head, meta, title precede it
		`<script src="/canvas.js" defer></script>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit-mode body missing %q:\n%s", want, body)
		}
	}

	// The two identical <p>s must carry distinct addresses — element identity,
	// never content matching.
	if !strings.Contains(body, `<p data-tb-el="6">Same text</p>`) || !strings.Contains(body, `<p data-tb-el="7">Same text</p>`) {
		t.Errorf("duplicated content must get distinct addresses:\n%s", body)
	}

	// Without a session the same URL serves the plain page: no stamps, no
	// editor script.
	anon := canvasRigGet(t, rig, "/s/"+slug+"/index.html?tb_edit=1", nil)
	if anon.Code != http.StatusOK {
		t.Fatalf("anonymous GET = %d, want 200", anon.Code)
	}
	if strings.Contains(anon.Body.String(), "data-tb-el") || strings.Contains(anon.Body.String(), "canvas.js") {
		t.Fatalf("edit-mode instrumentation leaked to an anonymous viewer:\n%s", anon.Body.String())
	}
}

func TestCanvasPage_RendersForOwner(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "canvas-owner2@example.com"

	mustWrite(t, ctx, st, slug, "index.html", canvasTestPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner})
	rig := newPrivateRig(t, st, snapshot.New(st, 0))

	res := canvasRigGet(t, rig, "/v2/workspace/"+slug, rig.session(t, owner, auth.RoleAdmin))
	if res.Code != http.StatusOK {
		t.Fatalf("GET /v2/workspace = %d, want 200: %s", res.Code, res.Body.String())
	}
	body := res.Body.String()
	for _, want := range []string{
		"tb_edit=1",          // the iframe loads the instrumented page
		`id="edit-form"`,     // command bar
		`id="run-feed"`,      // verdict feed slide-over
		"Whole site",         // default scope chip
		"/workspace/" + slug, // escape hatch to classic
	} {
		if !strings.Contains(body, want) {
			t.Errorf("canvas page missing %q", want)
		}
	}

	stranger := canvasRigGet(t, rig, "/v2/workspace/"+slug, rig.session(t, "stranger@example.com", auth.RoleAdmin))
	if stranger.Code != http.StatusNotFound {
		t.Fatalf("non-owner GET /v2/workspace = %d, want 404", stranger.Code)
	}
}
