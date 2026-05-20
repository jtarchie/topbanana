package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/bloomhollow/internal/snapshot"
)

// TestWorkspaceProgress_StatusStripUpdatesFromSSE drives the workspace
// status strip's SSE-fed DOM update flow in a real browser. The HTTP-level
// happy-path test consumes the /events SSE stream itself, so it catches a
// server-side regression where the events tracker stops emitting terminal
// statuses — but it can't catch a client-side regression in the workspace's
// EventSource handler (the JS that translates events into step-primary
// classes and "Your site is ready." text). This test does.
//
// Skips when Chrome isn't installed or MinIO env isn't set.
func TestWorkspaceProgress_StatusStripUpdatesFromSSE(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	chromePath := chromeExecPath()
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	runner := &stubRunner{title: "Progress", desc: "progress test"}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	slug := "wsprog-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Kick off the build via the form so the workspace renders in
	// ?building=1 mode (status strip enabled). The stub runner writes a
	// valid index.html and emits a write_file tool event; the build then
	// completes through the lint pass.
	form := url.Values{
		"template": {"blank"},
		"slug":     {slug},
		"prompt":   {"hello"},
	}
	// Issue the POST via the standard library before chromedp navigates.
	// Submitting through Chrome would also work but requires waiting on the
	// 303 redirect chain; this is simpler and the server-side path is
	// already covered by the HTTP e2e test.
	postBuildForm(t, httpSrv.URL, form)

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

	var statusText string
	var doneStepActive bool
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug+"?building=1"),
		chromedp.WaitVisible(`#status-strip`, chromedp.ByID),
		// Poll until the status message reports the terminal "ready" state.
		// If the SSE handler is broken the message will stay at "Building
		// your site…" until the deadline fires.
		chromedp.Poll(
			`(function(){
				var el = document.getElementById('status-msg');
				if (!el) return false;
				var t = el.textContent || '';
				return t.indexOf('ready') >= 0 ? t : false;
			})()`,
			&statusText,
			chromedp.WithPollingInterval(100*time.Millisecond),
		),
		chromedp.Evaluate(`(function(){
			var li = document.querySelector('[data-step="done"]');
			return !!(li && li.classList.contains('step-primary'));
		})()`, &doneStepActive),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}
	if !strings.Contains(strings.ToLower(statusText), "ready") {
		t.Errorf("status text never reached the ready state: %q", statusText)
	}
	if !doneStepActive {
		t.Errorf("status strip's 'done' step never became step-primary")
	}
}

// postBuildForm issues the form-encoded POST /build the workspace test
// kicks off. Sends Host=localhost and the test session cookie so the
// requireUser middleware accepts the request; doesn't follow redirects
// since we only care that the build was accepted and is in flight.
func postBuildForm(t *testing.T, base string, form url.Values) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/build", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST /build: %v", err)
	}
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(testSessionCookie)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("POST /build: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /build status: %d", resp.StatusCode)
	}
}
