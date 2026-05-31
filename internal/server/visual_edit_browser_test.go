package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestVisualEditCanvasSelection_BridgesToAIPanel exercises the GrapesJS-side
// half of the selection bridge: a drag-highlight inside the GrapesJS canvas
// iframe should populate the "Refine with AI" panel's selection preview, so
// the agent receives the same selection payload regardless of which editor
// the user is in. Without this test the bridge could silently fall back to
// editor.getSelected().toHTML() (whole-component selection) and the
// regression would look fine in HTTP responses.
//
// Loads GrapesJS from the unpkg CDN. The CDN-availability check runs in a
// short bounded poll so an offline CI environment short-circuits to skip
// rather than burn the full deadline.
func TestVisualEditCanvasSelection_BridgesToAIPanel(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	chromePath := chromeExecPath()
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	const marker = "Canvas drag highlight"
	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html><head><title>vis</title></head><body><h1 id="h">`+marker+`</h1></body></html>`,
		"text/html; charset=utf-8")

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	parentURL := strings.Replace(httpSrv.URL, "127.0.0.1", "localhost", 1)

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chromePath),
			chromedp.Flag("headless", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-gpu", true),
		)...,
	)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	// First navigate + set cookie. A separate shorter timeout wraps the
	// "did GrapesJS boot?" probe so a missing CDN skips fast instead of
	// chewing through the full deadline.
	navCtx, cancelNav := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelNav()
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: "localhost",
			Path:   "/",
		}}),
		chromedp.Navigate(parentURL+"/edit/"+slug+"/visual?page=index.html"),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp navigate failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp navigate: %v", err)
	}

	bootCtx, cancelBoot := context.WithTimeout(browserCtx, 15*time.Second)
	defer cancelBoot()
	var grapesReady bool
	err = chromedp.Run(bootCtx, chromedp.Poll(
		`(function(){
			if (!window.editor) return false;
			var d = window.editor.Canvas && window.editor.Canvas.getDocument && window.editor.Canvas.getDocument();
			if (!d) return false;
			var h = d.getElementById('h');
			return !!(h && h.textContent && h.textContent.indexOf(`+jsString(marker)+`) >= 0);
		})()`,
		&grapesReady,
		chromedp.WithPollingInterval(200*time.Millisecond),
	))
	if err != nil {
		// Treat boot-poll timeout as "CDN unreachable" rather than a
		// bridge failure. The HTTP-level e2e test still covers the
		// listener-injection contract.
		t.Skipf("GrapesJS did not finish booting within 15s (likely unpkg CDN unreachable): %v", err)
	}

	// Bridge half: programmatically drag-select the heading text inside
	// the canvas iframe, open the AI panel, and confirm the preview chip
	// shows the excerpt. We dispatch a selectionchange event after the
	// programmatic addRange because headless Chromium doesn't always emit
	// it for programmatic selection changes. A timeout here is a real
	// bridge failure.
	bridgeCtx, cancelBridge := context.WithTimeout(browserCtx, 15*time.Second)
	defer cancelBridge()
	var selPreview string
	var debug string
	err = chromedp.Run(bridgeCtx,
		chromedp.Click(`#refine-btn`, chromedp.ByQuery),
		chromedp.Evaluate(`(function(){
			var cdoc = window.editor.Canvas.getDocument();
			// GrapesJS sets user-select:none on the canvas body for
			// drag-and-drop; override it so the test's programmatic
			// Range.addRange actually creates a non-collapsed
			// selection.
			cdoc.body.style.userSelect = 'text';
			cdoc.body.style.webkitUserSelect = 'text';
			var h = cdoc.getElementById('h');
			h.style.userSelect = 'text';
			h.style.webkitUserSelect = 'text';
			var cwin = cdoc.defaultView;
			var r = cdoc.createRange();
			r.selectNodeContents(h);
			var s = cwin.getSelection();
			s.removeAllRanges();
			s.addRange(r);
			cdoc.dispatchEvent(new Event('selectionchange'));
		})()`, nil),
		chromedp.Poll(
			`(function(){
				var el = document.getElementById('ai-selection');
				var t = el ? el.textContent : '';
				return t.indexOf(`+jsString(marker)+`) >= 0 ? t : false;
			})()`,
			&selPreview,
			chromedp.WithPollingInterval(100*time.Millisecond),
		),
	)
	if err != nil {
		// Capture state for diagnosis without failing the whole run.
		_ = chromedp.Run(browserCtx, chromedp.Evaluate(`(function(){
			var cdoc = window.editor && window.editor.Canvas && window.editor.Canvas.getDocument();
			var sel = cdoc && cdoc.getSelection ? cdoc.getSelection().toString() : '';
			var aiEl = document.getElementById('ai-selection');
			return JSON.stringify({
				canvasSelText: sel,
				aiSelection: aiEl ? aiEl.textContent : null,
			});
		})()`, &debug))
		t.Fatalf("canvas selection bridge: %v\npreview=%q\ndebug=%s", err, selPreview, debug)
	}
	if !strings.Contains(selPreview, marker) {
		t.Errorf("Refine-with-AI preview did not pick up canvas drag selection.\ngot: %q\nwant substring: %q", selPreview, marker)
	}
}

// shouldSkipChrome reports whether a chromedp error should downgrade the
// test to a skip (Chrome unavailable / network unreachable) rather than a
// hard failure.
func shouldSkipChrome(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "chrome failed to start") ||
		strings.Contains(msg, "exec:") ||
		strings.Contains(msg, "context deadline exceeded")
}

// jsString quotes a string for embedding in a JS source snippet sent to
// chromedp.Evaluate. Quoting via %q is enough since the values used here
// are plain ASCII; we keep it tiny rather than pulling in encoding/json
// for one literal.
func jsString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\', '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
