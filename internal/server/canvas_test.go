package server_test

// The canvas (v2 workspace) rests on one contract: a page served with
// ?tb_edit=1 to an authorized caller carries element addresses and the
// selection script, and carries neither for anyone else. These tests pin
// that, plus the canvas page render itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

const canvasTestPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>t</title><link rel="stylesheet" href="/site.css"></head>
<body>
<h1>Hello</h1>
<p>Same text</p>
<p>Same text</p>
<img src="/assets/pic.png" alt="">
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
		`data-tb-el="5"`, // body — html, head, meta, title, link precede it
		`<script src="/canvas.js" defer></script>`,
		// Root-absolute references must be rebased onto the /s mount or the
		// page renders unstyled inside the canvas iframe.
		`href="/s/` + slug + `/site.css"`,
		`src="/s/` + slug + `/assets/pic.png"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("edit-mode body missing %q:\n%s", want, body)
		}
	}

	// The two identical <p>s must carry distinct addresses — element identity,
	// never content matching.
	if !strings.Contains(body, `<p data-tb-el="7">Same text</p>`) || !strings.Contains(body, `<p data-tb-el="8">Same text</p>`) {
		t.Errorf("duplicated content must get distinct addresses:\n%s", body)
	}

	// Every /s response — edit mode included — must carry the CSP sandbox:
	// the mount is same-origin with the admin UI, and without the opaque
	// origin a hosted site's own <script> runs with the admin's cookies.
	if got := res.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox allow-scripts") || !strings.Contains(got, "frame-ancestors 'self'") {
		t.Fatalf("edit-mode /s response missing the sandbox CSP, got %q", got)
	}

	// Without a session the same URL serves the plain page: no stamps, no
	// editor script — but still sandboxed.
	anon := canvasRigGet(t, rig, "/s/"+slug+"/index.html?tb_edit=1", nil)
	if anon.Code != http.StatusOK {
		t.Fatalf("anonymous GET = %d, want 200", anon.Code)
	}
	if strings.Contains(anon.Body.String(), "data-tb-el") || strings.Contains(anon.Body.String(), "canvas.js") {
		t.Fatalf("edit-mode instrumentation leaked to an anonymous viewer:\n%s", anon.Body.String())
	}
	if got := anon.Header().Get("Content-Security-Policy"); !strings.Contains(got, "sandbox allow-scripts") {
		t.Fatalf("anonymous /s response missing the sandbox CSP, got %q", got)
	}
}

// promptCaptureRunner records every prompt the build service hands the agent,
// so a test can assert what the resolved element-scope block contains.
type promptCaptureRunner struct {
	mu      sync.Mutex
	prompts []string
}

func (r *promptCaptureRunner) Run(_ context.Context, _ *store.Store, req build.RunRequest, _ func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	r.mu.Lock()
	r.prompts = append(r.prompts, req.Prompt)
	r.mu.Unlock()
	return agent.Usage{}, nil
}

func (r *promptCaptureRunner) Describe(context.Context, *store.Store, string, string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

func (r *promptCaptureRunner) first() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.prompts) == 0 {
		return ""
	}
	return r.prompts[0]
}

// canvasScopedPage is lint-clean (so the run completes without retries) and
// carries the duplicated paragraphs the scope must disambiguate. Element
// order: html 0, head 1, meta 2, meta 3, title 4, meta 5, link 6, body 7,
// h1 8, p 9, p 10.
const canvasScopedPage = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Scoped</title>
<meta name="description" content="A page for scope-resolution tests.">
<link rel="stylesheet" href="/app.css">
</head>
<body>
<h1>Hello</h1>
<p>Same text</p>
<p>Same text</p>
</body>
</html>`

// TestTextEdit_ReplacesExactlyTheAddressedNode is the write-side identity
// proof: two byte-identical paragraphs, and a direct edit addressed at the
// second must change only the second. Plus the guards: a stale expectation
// conflicts instead of clobbering, and typed markup is stored escaped.
func TestTextEdit_ReplacesExactlyTheAddressedNode(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "textedit-owner@example.com"

	mustWrite(t, ctx, st, slug, "index.html", canvasScopedPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: owner})
	rig := newPrivateRig(t, st, snapshot.New(st, 0))
	cookie := rig.session(t, owner, auth.RoleAdmin)

	postText := func(form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v2/text/"+slug, strings.NewReader(form.Encode()))
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		rig.handler.ServeHTTP(rec, req)
		return rec
	}

	// canvasScopedPage: the second duplicated <p> is element 10, its only
	// text node index 0.
	res := postText(url.Values{
		"page": {"index.html"}, "el": {"10"}, "text_index": {"0"},
		"text": {"Now different & <b>escaped</b>"}, "expect": {"Same text"},
	}, cookie)
	if res.Code != http.StatusOK {
		t.Fatalf("text edit = %d, want 200: %s", res.Code, res.Body.String())
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(obj.Content, "<p>Same text</p>") {
		t.Fatalf("the FIRST duplicate must be untouched:\n%s", obj.Content)
	}
	if !strings.Contains(obj.Content, "<p>Now different &amp; &lt;b&gt;escaped&lt;/b&gt;</p>") {
		t.Fatalf("the second duplicate must carry the new, escaped text:\n%s", obj.Content)
	}
	if strings.Count(obj.Content, "Same text") != 1 {
		t.Fatalf("exactly one duplicate should remain:\n%s", obj.Content)
	}

	// The stored text changed, so the original expectation now conflicts.
	res = postText(url.Values{
		"page": {"index.html"}, "el": {"10"}, "text_index": {"0"},
		"text": {"again"}, "expect": {"Same text"},
	}, cookie)
	if res.Code != http.StatusConflict || !strings.Contains(res.Body.String(), "reload") {
		t.Fatalf("stale expect: got %d %q, want 409 with reload guidance", res.Code, res.Body.String())
	}

	// Ownership gate: strangers get the slug-hiding 404.
	res = postText(url.Values{
		"page": {"index.html"}, "el": {"10"}, "text_index": {"0"},
		"text": {"x"}, "expect": {"x"},
	}, rig.session(t, "stranger@example.com", auth.RoleAdmin))
	if res.Code != http.StatusNotFound {
		t.Fatalf("non-owner text edit = %d, want 404", res.Code)
	}
}

// TestEditScope_ResolvedServerSideFromStoredSource pins the trust boundary:
// the canvas sends only an element address, and the markup the agent sees is
// resolved from the stored bytes — never client-supplied.
func TestEditScope_ResolvedServerSideFromStoredSource(t *testing.T) {
	st := storetest.New(t, 0)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", canvasScopedPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser})

	runner := &promptCaptureRunner{}
	handler := buildServerWithRunner(t, st, snapSvc, runner)

	postEdit := func(form url.Values) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/edit/"+slug, strings.NewReader(form.Encode()))
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(testSessionCookie)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// A stale address must be a clear user-facing error, not a mis-scoped run.
	res := postEdit(url.Values{
		"prompt": {"change it"}, "page": {""},
		"scope_el": {"99"}, "scope_page": {"index.html"},
	})
	if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "click the element again") {
		t.Fatalf("stale address: got %d %q, want 400 with reselect guidance", res.Code, res.Body.String())
	}

	// A valid address resolves to the stored markup of exactly that element.
	res = postEdit(url.Values{
		"prompt": {"make this paragraph bold"}, "page": {""},
		"scope_el": {"9"}, "scope_page": {"index.html"},
	})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("scoped edit POST = %d, want 303: %s", res.Code, res.Body.String())
	}

	deadline := time.Now().Add(10 * time.Second)
	for runner.first() == "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	prompt := runner.first()
	if prompt == "" {
		t.Fatal("agent never received a prompt")
	}
	for _, want := range []string{
		"element #9",
		"<p>Same text</p>",
		"THAT element only",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("agent prompt missing %q:\n%s", want, prompt)
		}
	}
	// The markup must be the stored bytes — no /s-mount rewriting, no stamps.
	if strings.Contains(prompt, "data-tb-el") || strings.Contains(prompt, "/s/"+slug) {
		t.Errorf("agent prompt leaked serve-time instrumentation:\n%s", prompt)
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
		// Page menu: lazy live thumbnails (src-less until opened) + new page.
		`data-thumb-src="/s/` + slug + `/index.html"`,
		"New page…",
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
