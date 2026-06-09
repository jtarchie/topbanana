package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestWorkspaceInsertImage_AddsRefToPrompt drives the workspace page's
// Images drawer Insert flow: open the drawer via the sidebar Browse button,
// pick an asset, click Insert, and assert that an `assets/<path>` markdown
// reference lands in the #prompt textarea. The workspace has two entry
// points to the drawer (sidebar Browse + prompt-side "Pick image…") and
// users reasonably expect Insert to work either way; this test pins that
// contract so the sidebar entry can't silently regress to a no-op.
func TestWorkspaceInsertImage_AddsRefToPrompt(t *testing.T) {
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

	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html><head><title>w</title></head><body><h1>hello</h1></body></html>`,
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
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

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

	navCtx, cancelNav := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelNav()
	err = chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: "localhost",
			Path:   "/",
		}}),
		chromedp.Navigate(parentURL+"/workspace/"+slug),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp navigate failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp navigate: %v", err)
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

	// Wait until the page's IIFE has wired up the drawer — the sidebar
	// Browse button is rendered at template time but the click handler is
	// attached only after TBImageDrawer.init runs.
	var drawerReady bool
	drive("drawer wiring", 10*time.Second, chromedp.Poll(
		`!!(window.TBImageDrawer && document.getElementById('open-image-drawer'))`,
		&drawerReady, chromedp.WithPollingInterval(100*time.Millisecond),
	))

	// Open via the SIDEBAR Browse button, not the prompt-side "Pick image…"
	// button. The bug is that Insert silently no-ops in this entry mode.
	var drawerOpen bool
	drive("open drawer (sidebar Browse)", 5*time.Second,
		chromedp.Evaluate(`document.getElementById('open-image-drawer').click()`, nil),
		chromedp.Poll(
			`document.getElementById('tb-drawer-panel').dataset.open === 'true'`,
			&drawerOpen, chromedp.WithPollingInterval(50*time.Millisecond),
		),
	)

	var gridCount int
	drive("asset grid did not render", 10*time.Second, chromedp.Poll(
		`document.querySelectorAll('#tb-drawer-grid .tb-drawer-card').length`,
		&gridCount, chromedp.WithPollingInterval(100*time.Millisecond),
	))

	var detailShown bool
	drive("show detail", 5*time.Second,
		chromedp.Evaluate(
			`document.querySelector('#tb-drawer-grid .tb-drawer-card').click()`, nil),
		chromedp.Poll(
			`!document.getElementById('tb-drawer-detail').hidden`,
			&detailShown, chromedp.WithPollingInterval(50*time.Millisecond),
		),
	)

	drive("click insert", 5*time.Second, chromedp.Evaluate(
		`document.getElementById('tb-drawer-insert').click()`, nil))

	// Assertion: the asset reference appears in the prompt textarea. The
	// workspace renders the reference as a backtick-quoted relative path
	// so the agent sees a literal `assets/<path>` mention.
	var promptValue string
	drive("prompt textarea did not pick up the asset reference", 5*time.Second, chromedp.Poll(
		`(function(){
			var ta = document.getElementById('prompt');
			if (!ta) return false;
			return ta.value.indexOf(`+jsString("`"+assetPath+"`")+`) >= 0 ? ta.value : false;
		})()`,
		&promptValue, chromedp.WithPollingInterval(50*time.Millisecond),
	))

	var drawerClosed bool
	drive("drawer did not close after Insert", 5*time.Second, chromedp.Poll(
		`document.getElementById('tb-drawer-panel').dataset.open === 'false'`,
		&drawerClosed, chromedp.WithPollingInterval(50*time.Millisecond),
	))

	// Edge case: no active cursor in the prompt. Seed text, force the
	// selection back to 0,0, and blur the textarea so it has no focus.
	// Then re-open the drawer and Insert. The snippet should APPEND to
	// the seeded text, not prepend before it — selectionStart=0 on an
	// unfocused textarea is the default, not a deliberate cursor.
	const seeded = "existing prompt"
	var appended string
	var gridReady bool
	drive("prepare unfocused textarea", 5*time.Second,
		chromedp.Evaluate(`(function(){
			var ta = document.getElementById('prompt');
			ta.value = `+jsString(seeded)+`;
			ta.setSelectionRange(0, 0);
			ta.blur();
		})()`, nil),
		chromedp.Evaluate(`document.getElementById('open-image-drawer').click()`, nil),
		chromedp.Poll(
			`document.querySelectorAll('#tb-drawer-grid .tb-drawer-card').length > 0`,
			&gridReady, chromedp.WithPollingInterval(100*time.Millisecond),
		),
		chromedp.Evaluate(
			`document.querySelector('#tb-drawer-grid .tb-drawer-card').click()`, nil),
		chromedp.Poll(
			`!document.getElementById('tb-drawer-detail').hidden`,
			&detailShown, chromedp.WithPollingInterval(50*time.Millisecond),
		),
		chromedp.Evaluate(`document.getElementById('tb-drawer-insert').click()`, nil),
		chromedp.Poll(
			`(function(){
				var ta = document.getElementById('prompt');
				return ta.value.indexOf(`+jsString("`"+assetPath+"`")+`) >= 0 ? ta.value : false;
			})()`,
			&appended, chromedp.WithPollingInterval(50*time.Millisecond),
		),
	)
	if !strings.HasPrefix(appended, seeded) {
		t.Errorf("snippet prepended ahead of seeded text instead of appending.\ngot:    %q\nwanted prefix: %q",
			appended, seeded)
	}
}
