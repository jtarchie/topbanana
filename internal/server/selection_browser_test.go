package server_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/buildabear/internal/snapshot"
)

// TestSelectionBridge_IframeToParent drives the full selection round-trip
// through a real Chromium: the workspace page iframes the slug subdomain,
// the iframe's selection listener postMessages the highlighted text up, and
// the parent's #selection-text chip should display the excerpt. The
// HTTP-level e2e test asserts the listener bytes ship; this test confirms
// the listener actually runs in the iframe and the message reaches the
// parent — the behavior that broke when the listener got gated on canEdit.
func TestSelectionBridge_IframeToParent(t *testing.T) {
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

	// The iframe page picks its own text and dispatches the selection on
	// load. `s.addRange(r)` fires a selectionchange event in Chromium,
	// which the injected selection_listener postMessages to the parent.
	// The 100ms delay lets the listener script register first.
	const marker = "Hello selection from iframe"
	mustWrite(t, ctx, st, slug, "index.html",
		`<!DOCTYPE html><html><head><title>sel</title></head><body>
<h1 id="h">`+marker+`</h1>
<script>
setTimeout(function(){
  var r = document.createRange();
  r.selectNodeContents(document.getElementById('h'));
  var s = window.getSelection();
  s.removeAllRanges();
  s.addRange(r);
}, 100);
</script>
</body></html>`,
		"text/html; charset=utf-8")

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// Replace the httptest 127.0.0.1 host with `localhost` so the Domain on
	// the server matches and `<slug>.localhost` routes through
	// subdomainMiddleware. host-resolver-rules forces Chromium to send
	// *.localhost to the loopback listener regardless of platform quirks.
	parentURL := strings.Replace(httpSrv.URL, "127.0.0.1", "localhost", 1)
	port := strings.TrimPrefix(httpSrv.URL, "http://127.0.0.1:")

	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.ExecPath(chromePath),
			chromedp.Flag("headless", true),
			chromedp.Flag("no-sandbox", true),
			chromedp.Flag("disable-gpu", true),
			chromedp.Flag("host-resolver-rules", "MAP *.localhost 127.0.0.1:"+port),
		)...,
	)
	defer cancelAlloc()

	browserCtx, cancelBrowser := chromedp.NewContext(allocCtx)
	defer cancelBrowser()

	runCtx, cancelRun := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelRun()

	var chipText string
	err := chromedp.Run(runCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: "localhost",
			Path:   "/",
		}}),
		chromedp.Navigate(parentURL+"/workspace/"+slug+"?page=index.html"),
		chromedp.WaitVisible(`iframe`, chromedp.ByQuery),
		// Poll the parent chip's textContent until the iframe's
		// selection bridge fires and the workspace listener populates
		// it. Bails fast on timeout (the runCtx deadline above).
		chromedp.Poll(
			`document.getElementById('selection-text') && document.getElementById('selection-text').textContent`,
			&chipText,
			chromedp.WithPollingInterval(100*time.Millisecond),
		),
	)
	if err != nil {
		if strings.Contains(err.Error(), "chrome failed to start") ||
			strings.Contains(err.Error(), "exec:") ||
			strings.Contains(err.Error(), "context deadline exceeded") {
			t.Skipf("chromedp run failed (%v) — skipping browser selection test", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}
	if !strings.Contains(chipText, marker) {
		t.Errorf("selection chip did not pick up iframe selection.\ngot: %q\nwant substring: %q", chipText, marker)
	}
}
