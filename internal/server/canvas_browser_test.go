package server_test

// Drives the v2 canvas in a real browser: the edit-mode iframe must carry
// element addresses, clicking an element must select it (halo + scope chip
// naming the element), and clicking the selected element again must step out
// to its parent. Only a browser runs the injected selection script and the
// postMessage bridge, so none of this is visible to server-side tests.
//
// Skips when Chrome isn't installed.

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

type canvasState struct {
	Stamped    int    `json:"stamped"`
	ScopeLabel string `json:"scopeLabel"`
	HaloShown  bool   `json:"haloShown"`
}

// canvasProbe reaches into the same-origin iframe, clicks the given selector,
// and reports the resulting selection state after the postMessage round-trip.
const canvasClickJS = `(function(sel){
	var doc = document.getElementById('canvas-frame').contentDocument;
	var el = doc.querySelector(sel);
	if (el) el.dispatchEvent(new MouseEvent('click', {bubbles: true, cancelable: true}));
	return !!el;
})(%q)`

const canvasStateJS = `(function(){
	var frame = document.getElementById('canvas-frame');
	var doc = frame.contentDocument;
	var halo = false;
	if (doc) {
		var boxes = doc.documentElement.querySelectorAll('div[style*="2147483646"]');
		boxes.forEach(function(b){ if (b.style.borderStyle === 'solid' && b.style.display !== 'none') halo = true; });
	}
	return {
		stamped: doc ? doc.querySelectorAll('[data-tb-el]').length : -1,
		scopeLabel: document.getElementById('scope-label').textContent,
		haloShown: halo
	};
})()`

func TestCanvas_SelectElementInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", canvasTestPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

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

	host := strings.TrimPrefix(httpSrv.URL, "http://")
	host = strings.SplitN(host, ":", 2)[0]

	navCtx, cancelNav := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelNav()

	var clicked bool
	var afterFirst, afterSecond canvasState
	var shot []byte
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.EmulateViewport(1440, 900),
		chromedp.Navigate(httpSrv.URL+"/v2/workspace/"+slug),
		// Wait until the edit-mode iframe has loaded and its script announced
		// itself by stamping elements the parent can see.
		chromedp.Poll(`(function(){
			var f = document.getElementById('canvas-frame');
			return !!(f && f.contentDocument && f.contentDocument.querySelector('[data-tb-el]') && f.contentWindow.__tbCanvas);
		})()`, nil, chromedp.WithPollingTimeout(15*time.Second)),
		chromedp.Evaluate(canvasClickFor("h1"), &clicked),
		chromedp.Sleep(300*time.Millisecond), // postMessage round-trip
		chromedp.Evaluate(canvasStateJS, &afterFirst),
		chromedp.FullScreenshot(&shot, 90),
		// Clicking the selected element again steps out to its parent (body).
		chromedp.Evaluate(canvasClickFor("h1"), &clicked),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(canvasStateJS, &afterSecond),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}

	if dir := os.Getenv("TB_BROWSER_SHOT_DIR"); dir != "" && len(shot) > 0 {
		_ = os.WriteFile(dir+"/canvas.png", shot, 0o644)
	}

	if afterFirst.Stamped < 5 {
		t.Fatalf("iframe stamped elements = %d, want the whole page addressed", afterFirst.Stamped)
	}
	if !strings.Contains(afterFirst.ScopeLabel, "h1") {
		t.Fatalf("scope chip after clicking h1 = %q, want it to name the element", afterFirst.ScopeLabel)
	}
	if !afterFirst.HaloShown {
		t.Fatal("selection halo not visible after click")
	}
	if !strings.Contains(afterSecond.ScopeLabel, "body") {
		t.Fatalf("second click must step selection out to the parent, got %q", afterSecond.ScopeLabel)
	}
}

func canvasClickFor(sel string) string {
	return strings.Replace(canvasClickJS, "%q", `'`+sel+`'`, 1)
}
