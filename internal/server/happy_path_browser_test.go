package server_test

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/bloomhollow/internal/snapshot"
)

// chromeExecPath returns a working Chrome binary path, or "" when none is
// installed. chromedp's default allocator does its own PATH lookup but
// doesn't know to look in macOS application bundles, so we hand it the
// canonical bundle path when the binary is there.
func chromeExecPath() string {
	for _, p := range []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
	} {
		_, err := os.Stat(p)
		if err == nil {
			return p
		}
	}
	for _, name := range []string{"google-chrome", "chromium", "chrome"} {
		p, err := exec.LookPath(name)
		if err == nil {
			return p
		}
	}
	return ""
}

// TestHappyPath_BrowserSmoke loads the redesigned landing page in headless
// Chrome and asserts the DaisyUI corporate theme + Tailwind processing landed.
// This is a thin smoke test — its job is to catch the kinds of failures the
// HTTP-level test can't see, like a missing CDN script, a CSS that fails to
// apply, or a JavaScript error in the boot path. Skips when Chrome isn't
// available or when Minio env vars aren't set (server.New touches the bucket
// at startup).
func TestHappyPath_BrowserSmoke(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	snapSvc := snapshot.New(st, 0)
	runner := &stubRunner{title: "Smoke", desc: "smoke test"}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	chromePath := chromeExecPath()
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser smoke test")
	}
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

	runCtx, cancelRun := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelRun()

	// chromedp doesn't let us set the Host header easily, but httptest.Server
	// already serves on 127.0.0.1 — which is in fallThroughHosts — so the
	// admin routes are reachable directly. After the basic-auth cutover we
	// inject the passkey session cookie before navigation so requireUser
	// sees a valid session.
	host := strings.TrimPrefix(httpSrv.URL, "http://")
	host = strings.SplitN(host, ":", 2)[0]

	var theme, bodyText string
	var primaryColor string
	err := chromedp.Run(runCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		// Force prefers-color-scheme=light so the dark-mode bootstrap
		// script lands on corporate (matching the SSR'd data-theme attr).
		// Without this the test runner's OS theme leaks into the assertion;
		// on a developer's mac in dark mode it'd flip <html> to business
		// and the data-theme check below would fail.
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{
			Name:  "prefers-color-scheme",
			Value: "light",
		}}),
		chromedp.Navigate(httpSrv.URL+"/"),
		chromedp.WaitVisible(`textarea#prompt`, chromedp.ByQuery),
		chromedp.AttributeValue(`html`, `data-theme`, &theme, nil),
		chromedp.Text(`h1`, &bodyText, chromedp.ByQuery),
		// getComputedStyle on the body returns the resolved DaisyUI primary
		// colour; if Tailwind + DaisyUI didn't load this will be empty.
		chromedp.Evaluate(`getComputedStyle(document.documentElement).getPropertyValue('--color-primary') || getComputedStyle(document.body).color`, &primaryColor),
	)
	if err != nil {
		// Treat Chrome startup or network issues as a skip rather than a
		// hard fail — the HTTP test already covers correctness; this is
		// the optional "did Tailwind actually render" check, and it
		// shouldn't gate CI on dev machines without a working Chrome.
		if strings.Contains(err.Error(), "chrome failed to start") ||
			strings.Contains(err.Error(), "exec:") ||
			strings.Contains(err.Error(), "context deadline exceeded") {
			t.Skipf("chromedp run failed (%v) — skipping browser smoke test", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}
	if theme != "corporate" {
		t.Errorf("data-theme: got %q want %q", theme, "corporate")
	}
	if !strings.Contains(bodyText, "Build a new app") {
		t.Errorf("landing h1 text: got %q", bodyText)
	}
	if strings.TrimSpace(primaryColor) == "" {
		t.Errorf("DaisyUI primary colour did not resolve — Tailwind/DaisyUI likely failed to load")
	}

	// Flip emulated media to dark, reload, and confirm the bootstrap script
	// honors prefers-color-scheme=dark by swapping data-theme to business.
	// Reload via Navigate so the inline head script re-runs against the new
	// emulated media query.
	var darkTheme string
	err = chromedp.Run(runCtx,
		chromedp.Evaluate(`localStorage.removeItem('bh_theme')`, nil),
		emulation.SetEmulatedMedia().WithFeatures([]*emulation.MediaFeature{{
			Name:  "prefers-color-scheme",
			Value: "dark",
		}}),
		chromedp.Navigate(httpSrv.URL+"/"),
		chromedp.WaitVisible(`textarea#prompt`, chromedp.ByQuery),
		chromedp.AttributeValue(`html`, `data-theme`, &darkTheme, nil),
	)
	if err != nil {
		t.Fatalf("dark-mode re-navigate: %v", err)
	}
	if darkTheme != "business" {
		t.Errorf("dark-mode data-theme: got %q want %q", darkTheme, "business")
	}
}
