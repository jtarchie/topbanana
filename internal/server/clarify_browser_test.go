package server_test

import (
	"context"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestClarify_QuestionCardInBrowser drives the ask_user question card in a real
// browser. The HTTP-level TestClarify_EndToEnd proves the server side of the
// loop; it can't catch a client-side regression in workspace.html's
// renderQuestionCard / postClarify / removeQuestionCard handlers. This does: it
// waits for the card to render from the SSE PhaseAsk event, clicks "Use this
// suggestion", and confirms the card is removed (PhaseAnswer) and the build
// completes — while asserting the agent goroutine received the recommendation.
//
// Skips when Chrome isn't installed.
func TestClarify_QuestionCardInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath()
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	runner := &questionRunner{title: "Clarify", desc: "clarify browser test", answered: make(chan string, 1)}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	slug := "clfyui-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// Kick the build so the workspace renders in ?building=1 mode (status strip
	// enabled). The stub agent asks a question and blocks until /clarify answers.
	postBuildForm(t, httpSrv.URL, url.Values{
		"template": {"blank"},
		"slug":     {slug},
		"prompt":   {"hello"},
	})

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

	var questionText string
	var cardGone bool
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug+"?building=1"),
		chromedp.WaitVisible(`#status-strip`, chromedp.ByID),
		// The question card renders from the SSE PhaseAsk event.
		chromedp.WaitVisible(`[data-qid]`, chromedp.ByQuery),
		chromedp.Text(`[data-qid] h3`, &questionText, chromedp.ByQuery, chromedp.NodeVisible),
		// Click "Use this suggestion" — the .btn-primary inside the card — which
		// POSTs the recommendation to /clarify via postClarify().
		chromedp.Click(`[data-qid] .btn-primary`, chromedp.ByQuery),
		// The PhaseAnswer event triggers removeQuestionCard(); poll until it's gone.
		chromedp.Poll(
			`(function(){ return document.querySelector('[data-qid]') === null; })()`,
			&cardGone,
			chromedp.WithPollingInterval(100*time.Millisecond),
		),
		// With the answer delivered the build completes and reveals the success panel.
		chromedp.WaitVisible(`#build-success`, chromedp.ByID),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}

	if !strings.Contains(strings.ToLower(questionText), "headline") {
		t.Errorf("question card heading = %q, want it to contain the question text", questionText)
	}
	if !cardGone {
		t.Error("question card was never removed after answering")
	}

	// Clicking "Use this suggestion" must deliver the recommendation to the agent.
	select {
	case got := <-runner.answered:
		if got != clarifyRecommendation {
			t.Errorf("agent received answer %q, want the recommendation %q", got, clarifyRecommendation)
		}
	case <-time.After(5 * time.Second):
		t.Error("agent goroutine never received an answer via /clarify")
	}
}
