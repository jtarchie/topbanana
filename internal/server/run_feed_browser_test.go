package server_test

// Renders the workspace run feed in a real browser: the panel must hydrate
// from /runs/:slug with the composer leading, the newest run as a full
// verdict card — including the amber "finished without changing anything"
// card with the agent's own explanation, the state the 2026-08-25 incident
// shipped without — and older runs collapsed to one-line rows. Server-side
// tests can't see this: the feed is built entirely by the page's own JS.
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

type runFeedState struct {
	Items             int    `json:"items"`
	CollapsedRows     int    `json:"collapsedRows"`
	UndoForms         int    `json:"undoForms"`
	ComposerAboveFeed bool   `json:"composerAboveFeed"`
	Text              string `json:"text"`
}

const runFeedProbe = `(function(){
	var feed = document.getElementById('run-feed');
	if (!feed) return { items: -1, collapsedRows: 0, undoForms: 0, composerAboveFeed: false, text: '' };
	// Expand the collapsed history so innerText covers older verdicts too.
	feed.querySelectorAll('details').forEach(function (d) { d.open = true; });
	var form = document.getElementById('edit-form');
	return {
		items: feed.children.length,
		collapsedRows: feed.querySelectorAll('details').length,
		undoForms: feed.querySelectorAll('form.js-confirm').length,
		// The composer must lead the panel: it precedes the feed in DOM order.
		composerAboveFeed: !!(form && (form.compareDocumentPosition(feed) & Node.DOCUMENT_POSITION_FOLLOWING)),
		text: feed.innerText
	};
})()`

func TestWorkspaceRunFeed_RendersVerdictsInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<html><head><title>t</title></head><body><h1>hi</h1></body></html>", "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	now := time.Now().UTC()
	// Older run: the incident shape — completed, read everything, changed
	// nothing, and said why.
	seedRunTranscriptRaw(t, st, slug, "edit", "make this line white", now.Add(-9*time.Minute), nil, "the text is already white")
	// Newer run: a real change whose transcript names its own pre-run
	// snapshot — the identity one-click Undo restores.
	snap, err := snapSvc.Create(ctx, slug, snapshot.ReasonEdit)
	if err != nil {
		t.Fatalf("create snapshot: %v", err)
	}
	seedRunTranscriptWithSnapshot(t, st, slug, "edit", "remove the word occasionally", now.Add(-2*time.Minute), []string{"index.html", "site.css"}, "done", snap.Key)

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

	var state runFeedState
	var shot []byte
	err = chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		// Desktop width: the xl two-column layout (composer left, sticky
		// preview right) is the layout under test.
		chromedp.EmulateViewport(1440, 900),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug),
		// The feed hydrates from a fetch; wait for its first entry.
		chromedp.WaitVisible(`#run-feed > *`, chromedp.ByQuery),
		chromedp.Evaluate(runFeedProbe, &state),
		chromedp.FullScreenshot(&shot, 90),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}

	if dir := os.Getenv("TB_BROWSER_SHOT_DIR"); dir != "" && len(shot) > 0 {
		_ = os.WriteFile(dir+"/run-feed.png", shot, 0o644)
	}

	if state.Items != 2 {
		t.Fatalf("feed items = %d, want 2 (latest card + earlier-changes block)", state.Items)
	}
	if state.CollapsedRows != 1 {
		t.Fatalf("collapsed history rows = %d, want 1", state.CollapsedRows)
	}
	if state.UndoForms != 1 {
		t.Fatalf("undo forms = %d, want exactly 1 (newest run only)", state.UndoForms)
	}
	if !state.ComposerAboveFeed {
		t.Fatal("composer must precede the feed — describing a change is the primary action")
	}
	for _, want := range []string{
		"make this line white",
		"Finished without changing anything",
		"the text is already white",
		"Edit request & retry",
		"remove the word occasionally",
		"Updated your Home page and your site's styling",
		"Undo this change",
		"Earlier changes",
	} {
		if !strings.Contains(state.Text, want) {
			t.Errorf("feed text missing %q\n--- feed text ---\n%s", want, state.Text)
		}
	}
}
