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

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestCanvasProgress_ReattachesToRunFromSSE drives the canvas's SSE-fed
// progress flow in a real browser. The HTTP-level happy-path test consumes
// the /events stream itself, so it catches a server-side regression where the
// tracker stops emitting terminal statuses — but not a client-side one in the
// canvas's EventSource handler. Three things must hold across a run the page
// did not start: it reattaches (composer disabled on load), the terminal event
// releases the composer, and the verdict is announced honestly. A break in any
// of them strands the owner on a page that looks permanently busy.
//
// Skips when Chrome isn't installed or MinIO env isn't set.
func TestCanvasProgress_ReattachesToRunFromSSE(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
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

	// Kick off the build via the form so the canvas renders in ?building=1
	// mode and reattaches to it. The stub runner writes a valid index.html and
	// emits a write_file tool event; the build then completes through the lint
	// pass.
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

	var toastText string
	var composerEnabled bool
	var changesOpen bool
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		// ?building=1 puts the page in reattach posture: composer disabled,
		// SSE stream opened for a run this page did not start.
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug+"?building=1"),
		chromedp.WaitVisible(`#canvas-status`, chromedp.ByID),
		// The toast is the completion signal on a canvas that otherwise just
		// reloads the frame under the user. Deliberately the first thing
		// polled: the stub runner can finish inside a single polling interval,
		// so anything asserting the *disabled* state first would race it.
		//
		// The stub writes index.html straight to the store rather than through
		// the agent's recorded tool path, so the run's verdict is honestly
		// empty — and this asserts the honest branch: a completed run that
		// changed nothing must say so and open Changes, never celebrate.
		chromedp.Poll(
			`(function(){
				var t = document.getElementById('toast');
				var s = t ? (t.textContent || '') : '';
				return s.trim() !== '' ? s : false;
			})()`,
			&toastText, chromedp.WithPollingInterval(100*time.Millisecond),
		),
		// …and the terminal event released the composer it disabled on load.
		// A regression here strands the owner on a page that looks busy
		// forever, with no way to ask for the next change.
		chromedp.Poll(
			`document.getElementById('prompt').disabled === false`,
			&composerEnabled, chromedp.WithPollingInterval(100*time.Millisecond),
		),
		chromedp.Poll(
			`document.getElementById('panel-changes').getAttribute('aria-hidden') === 'false'`,
			&changesOpen, chromedp.WithPollingInterval(100*time.Millisecond),
		),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}
	if !strings.Contains(strings.ToLower(toastText), "without changing anything") {
		t.Errorf("completion toast = %q, want the no-op verdict for a run that recorded no edits", toastText)
	}
	if !composerEnabled {
		t.Error("composer was never re-enabled after the run finished")
	}
	if !changesOpen {
		t.Error("Changes panel did not open to explain the no-op run")
	}
}

// postBuildForm issues the form-encoded POST /build the canvas tests
// kick off. Sends Host=localhost and the test session cookie so the
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
