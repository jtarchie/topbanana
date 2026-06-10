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

// TestVisualEditInsertImage_AddsImgComponent drives the full visual-editor
// Insert flow: open the Images drawer, pick a seeded asset, click Insert,
// and confirm (a) the GrapesJS canvas picks up an <img> with the asset's
// alt text and src, and (b) the drawer closes. The host page's onInsert
// callback (visual_edit.html) swallows exceptions into a status string, so
// "click did nothing" can only be caught by asserting on the editor HTML;
// this test is the regression line for that path.
//
// Loads GrapesJS from the unpkg CDN. The boot poll skips on timeout so an
// offline CI run short-circuits rather than reporting a bridge failure.
func TestVisualEditInsertImage_AddsImgComponent(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath()
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	const marker = "Insert image canvas"
	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html><head><title>vis</title></head><body><h1 id="h">`+marker+`</h1></body></html>`,
		"text/html; charset=utf-8")

	const assetPath = "assets/photo.png"
	const altText = "kitten"
	err := st.Write(ctx, slug, assetPath, string(mustTinyPNG(t)), "image/png", map[string]string{
		"alt":         altText,
		"description": "a kitten",
	})
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}

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

	// Phase 1: navigate + set session cookie.
	navCtx, cancelNav := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelNav()
	err = chromedp.Run(navCtx,
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

	// Phase 2: wait for GrapesJS to boot. Skip on timeout — unpkg unreachable.
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
		t.Skipf("GrapesJS did not finish booting within 15s (likely unpkg CDN unreachable): %v", err)
	}

	drive := func(label string, timeout time.Duration, actions ...chromedp.Action) {
		t.Helper()
		pctx, cancel := context.WithTimeout(browserCtx, timeout)
		defer cancel()
		runErr := chromedp.Run(pctx, actions...)
		if runErr != nil {
			t.Fatalf("%s: %v\n%s", label, runErr, dumpInsertState(t, browserCtx))
		}
	}

	// Phase 3: open the Images drawer.
	var drawerOpen bool
	drive("open drawer", 5*time.Second,
		chromedp.Click(`#images-btn`, chromedp.ByQuery),
		chromedp.Poll(
			`document.getElementById('tb-drawer-panel').dataset.open === 'true'`,
			&drawerOpen, chromedp.WithPollingInterval(50*time.Millisecond),
		),
	)

	// Phase 4: wait for the asset grid to render. The drawer fetches
	// /assets/:slug on open; the seeded photo should land in one card.
	var gridCount int
	drive("asset grid did not render", 10*time.Second, chromedp.Poll(
		`document.querySelectorAll('#tb-drawer-grid .tb-drawer-card').length`,
		&gridCount, chromedp.WithPollingInterval(100*time.Millisecond),
	))

	// Phase 5: open the detail view by clicking the first card. Use a JS
	// dispatched click rather than chromedp.Click — the card is a <button>
	// with an inner lazy-loaded <img>, and chromedp's "click at coords"
	// can race the image layout. Dispatching directly on the button is
	// what a real keyboard activation does anyway.
	var detailShown bool
	drive("show detail", 5*time.Second,
		chromedp.Evaluate(
			`document.querySelector('#tb-drawer-grid .tb-drawer-card').click()`, nil,
		),
		chromedp.Poll(
			`!document.getElementById('tb-drawer-detail').hidden`,
			&detailShown, chromedp.WithPollingInterval(50*time.Millisecond),
		),
	)

	// Phase 6: click Insert. JS dispatch for the same reason as the card
	// click above — direct activation, no coord-resolution race.
	drive("click insert", 5*time.Second, chromedp.Evaluate(
		`document.getElementById('tb-drawer-insert').click()`, nil))

	// Assertion 1: editor.getHtml() picks up the new <img> with the asset
	// path and alt text. This is the load-bearing assertion — if it stays
	// false, "click did nothing" is real.
	var editorHTML string
	drive("editor canvas did not pick up the inserted <img>", 10*time.Second, chromedp.Poll(
		`(function(){
			var h = window.editor && window.editor.getHtml ? window.editor.getHtml() : '';
			return (h.indexOf('src="`+assetPath+`"') >= 0 && h.indexOf('alt="`+altText+`"') >= 0) ? h : false;
		})()`,
		&editorHTML, chromedp.WithPollingInterval(100*time.Millisecond),
	))

	// Assertion 2: drawer closes after a successful insert. A silent
	// exception in onInsert would leave the drawer open via the status-line
	// error path (image_drawer.js:257).
	var drawerClosed bool
	drive("drawer did not close after Insert", 5*time.Second, chromedp.Poll(
		`document.getElementById('tb-drawer-panel').dataset.open === 'false'`,
		&drawerClosed, chromedp.WithPollingInterval(50*time.Millisecond),
	))

	// Assertion 3: the inserted <img> actually renders in the canvas
	// iframe. editor.getHtml() carrying the correct src is necessary but
	// not sufficient — if the relative path resolves against the editor's
	// own host (not the site's subdomain), the image is broken and the
	// user sees "nothing happened" even though the HTML is correct.
	// naturalWidth > 0 means the browser successfully loaded the bytes.
	var imgRendered bool
	drive("inserted <img> did not render in the canvas iframe (broken image)", 5*time.Second, chromedp.Poll(
		`(function(){
			var cdoc = window.editor.Canvas.getDocument();
			var imgs = cdoc.querySelectorAll('img');
			for (var i = 0; i < imgs.length; i++) {
				if (imgs[i].src.indexOf(`+jsString(assetPath)+`) >= 0 &&
				    imgs[i].complete && imgs[i].naturalWidth > 0) return true;
			}
			return false;
		})()`,
		&imgRendered, chromedp.WithPollingInterval(100*time.Millisecond),
	))
}

// dumpInsertState captures the in-page state we need to triage which step
// of the Insert flow broke. Best-effort: the caller has already decided to
// fail, so an evaluate error here just means the diagnostic is empty.
func dumpInsertState(t *testing.T, ctx context.Context) string {
	t.Helper()
	var dump string
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = chromedp.Run(probeCtx, chromedp.Evaluate(`(function(){
		try {
			var panel = document.getElementById('tb-drawer-panel');
			var detail = document.getElementById('tb-drawer-detail');
			var status = document.getElementById('tb-drawer-status');
			var grid = document.getElementById('tb-drawer-grid');
			var selected = (window.TBImageDrawer && window.TBImageDrawer.selected) || null;
			return JSON.stringify({
				editorHtml: (window.editor && window.editor.getHtml) ? window.editor.getHtml() : null,
				panelOpen: panel ? panel.dataset.open : null,
				detailHidden: detail ? detail.hidden : null,
				drawerStatus: status ? status.textContent : null,
				gridCount: grid ? grid.querySelectorAll('.tb-drawer-card').length : null,
				selectedPath: selected ? selected.path : null,
			});
		} catch (e) { return 'dump-failed: ' + e; }
	})()`, &dump))
	return "diagnostic: " + dump
}
