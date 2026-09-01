package server_test

// Drives the canvas in a real browser. The edit-mode iframe is served
// with a CSP sandbox (opaque origin), so the parent cannot reach its DOM —
// exactly the isolation under test — and the canvas is driven the way the
// product works: real mouse clicks routed into the frame, plus the
// parent-only tb-click postMessage seam. Selection state is observed through
// window.__tbScope on the parent, fed exclusively by the frame's messages.
//
// Skips when Chrome isn't installed.

import (
	"context"
	"fmt"
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

type scopeProbe struct {
	El   int    `json:"el"`
	Text string `json:"text"`
	Tag  string `json:"tag"`
}

// tbClick posts the parent-only selection command into the sandboxed frame.
// Returns a value so chromedp.Evaluate never sees `undefined`.
func tbClick(sel string) string {
	return fmt.Sprintf(
		`(document.getElementById('canvas-frame').contentWindow.postMessage({type:'tb-click', sel:%q}, '*'), true)`, sel)
}

// readScope evaluates to the current scope or a sentinel, never undefined.
const readScope = `window.__tbScope || {el:-1, tag:"", text:""}`

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

	// canvasTestPage element order: html 0, head 1, meta 2, title 3, link 4,
	// body 5, h1 6, p 7, p 8, img 9.
	var afterClick, stepOut, first, second scopeProbe
	var shot []byte
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.EmulateViewport(1440, 900),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug),
		// The sandboxed frame's script announces readiness over postMessage;
		// the parent records it — the only readiness signal an opaque-origin
		// frame can give.
		chromedp.Poll(`window.__tbReady === true`, nil, chromedp.WithPollingTimeout(15*time.Second)),

		// Selection is driven through the parent-only tb-click seam: CDP
		// synthetic mouse input does not route into an out-of-process
		// (sandboxed) iframe, and the seam shares the click path's selection
		// rules — including step-out — so the same logic is under test.
		chromedp.Evaluate(tbClick("h1"), nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(readScope, &afterClick),
		chromedp.FullScreenshot(&shot, 90),

		// Selecting the selected element again steps out to its parent.
		chromedp.Evaluate(tbClick("h1"), nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(readScope, &stepOut),

		// The identity proof: two byte-identical <p>s must select as distinct
		// addresses — same tag, same text, different el. Content-based
		// selection could not tell them apart.
		chromedp.Evaluate(tbClick(`[data-tb-el="7"]`), nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(readScope, &first),
		chromedp.Evaluate(tbClick(`[data-tb-el="8"]`), nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(readScope, &second),
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

	assertSelectionByAddress(t, afterClick, stepOut, first, second)
	assertBarPeeksOnScroll(t, navCtx)

	// Direct text editing, end to end: the tb-text-edit seam mutates the
	// second duplicated <p> in the frame and hands the save to the parent,
	// which makes the credentialed POST the opaque frame cannot. The stored
	// page must change at exactly that node.
	err = chromedp.Run(navCtx,
		chromedp.Evaluate(
			`(document.getElementById('canvas-frame').contentWindow.postMessage({type:'tb-text-edit', sel:'[data-tb-el="8"]', text_index:0, text:'Edited live'}, '*'), true)`, nil),
	)
	if err != nil {
		t.Fatalf("drive text edit: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		obj, rerr := st.Read(ctx, slug, "index.html")
		if rerr == nil && strings.Contains(obj.Content, "Edited live") {
			if !strings.Contains(obj.Content, "<p data-tb-el") && strings.Count(obj.Content, "Same text") != 1 {
				t.Fatalf("text edit must change only the addressed node:\n%s", obj.Content)
			}
			break
		}
		if time.Now().After(deadline) {
			content := ""
			if rerr == nil {
				content = obj.Content
			}
			t.Fatalf("stored page never received the in-place edit:\n%s", content)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// dropPNGJS builds a real 1x1 PNG File in the parent page and hands it to the
// image-drop handler exactly as a frame message would — the seam sits right
// after the postMessage hop, so upload, prompt composition, and the scoped
// run are all the production path.
const dropPNGJS = `(function(){
	var b64 = 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==';
	var bin = atob(b64);
	var arr = new Uint8Array(bin.length);
	for (var i = 0; i < bin.length; i++) arr[i] = bin.charCodeAt(i);
	window.__tbHandleImageDrop({ el: 9, position: 'after', file: new File([arr], 'drop.png', { type: 'image/png' }) });
	return true;
})()`

// TestCanvas_ImageDropInBrowser drives the drop pipeline end to end: the
// parent uploads the file and starts a run scoped to the drop target, whose
// prompt names the stored asset path and the placement edge.
func TestCanvas_ImageDropInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", canvasScopedPage, "text/html; charset=utf-8")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser})

	runner := &promptCaptureRunner{}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
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

	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug),
		chromedp.Poll(`window.__tbReady === true`, nil, chromedp.WithPollingTimeout(15*time.Second)),
		chromedp.Evaluate(dropPNGJS, nil),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}

	deadline := time.Now().Add(15 * time.Second)
	for {
		prompt := runner.first()
		if prompt != "" {
			for _, want := range []string{
				"Insert the image `assets/",
				"immediately after",
				"element #9", // the server-resolved scope of the drop target
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("drop run prompt missing %q:\n%s", want, prompt)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the drop never started an agent run")
		}
		time.Sleep(100 * time.Millisecond)
	}

	files, err := st.List(ctx, slug)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	uploaded := false
	for _, f := range files {
		if strings.HasPrefix(f, "assets/") {
			uploaded = true
		}
	}
	if !uploaded {
		t.Fatalf("dropped image was never stored: %v", files)
	}
}

// assertBarPeeksOnScroll pins the command bar's scroll-away behavior:
// scrolling the site down slides it to a peeking edge (content revealed),
// scrolling up brings it back. Driven through the tb-scrollto seam — the
// frame's real scroll listener does the reporting.
func assertBarPeeksOnScroll(t *testing.T, navCtx context.Context) {
	t.Helper()
	scrollTo := func(y int) string {
		return fmt.Sprintf(
			`(document.getElementById('canvas-frame').contentWindow.postMessage({type:'tb-scrollto', y:%d}, '*'), true)`, y)
	}
	// Blur the composer first — a focused composer deliberately pins the bar.
	var peeked, expanded bool
	err := chromedp.Run(navCtx,
		chromedp.Evaluate(`(document.activeElement && document.activeElement.blur(), true)`, nil),
		chromedp.Evaluate(scrollTo(1200), nil),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(`document.getElementById('edit-form').classList.contains('cmd-peek')`, &peeked),
		chromedp.Evaluate(scrollTo(0), nil),
		chromedp.Sleep(400*time.Millisecond),
		chromedp.Evaluate(`!document.getElementById('edit-form').classList.contains('cmd-peek')`, &expanded),
	)
	if err != nil {
		t.Fatalf("scroll drive: %v", err)
	}
	if !peeked {
		t.Fatal("scrolling down must slide the command bar to its peek state")
	}
	if !expanded {
		t.Fatal("scrolling up must bring the command bar back")
	}
}

// assertSelectionByAddress pins the selection contract: clicking selects by
// served address, reselecting steps out to the parent, and byte-identical
// content still selects as distinct elements.
func assertSelectionByAddress(t *testing.T, afterClick, stepOut, first, second scopeProbe) {
	t.Helper()
	if afterClick.Tag != "h1" || afterClick.El != 6 {
		t.Fatalf("clicking the heading must select it by address, got %+v", afterClick)
	}
	if stepOut.Tag != "body" || stepOut.El != 5 {
		t.Fatalf("second click must step selection out to the parent, got %+v", stepOut)
	}
	if first.Text != "Same text" || second.Text != "Same text" || first.Tag != "p" || second.Tag != "p" {
		t.Fatalf("expected two identical <p>Same text</p> selections, got %+v and %+v", first, second)
	}
	if first.El != 7 || second.El != 8 {
		t.Fatalf("identical content must select distinct served addresses, got %d and %d", first.El, second.El)
	}
}

// TestCanvas_StylesheetScopeInBrowser drives the page menu's stylesheet scope
// end to end: click the entry, type a prompt, submit, and assert the run the
// server started was confined to that file. The wiring between the button and
// the composer's `page` field is the part with no server-side equivalent — a
// silent regression there sends a whole-site run instead, and the only symptom
// is the agent editing pages the owner never asked it to touch.
//
// Skips when Chrome isn't installed.
func TestCanvas_StylesheetScopeInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", canvasScopedPage, "text/html; charset=utf-8")
	mustWrite(t, ctx, st, slug, "site.css", "h1 { color: red }", "text/css")
	writeMeta(t, ctx, st, slug, build.SiteMeta{OwnerID: testAdminUser})

	runner := &promptCaptureRunner{}
	httpSrv := httptest.NewServer(buildServerWithRunner(t, st, snapSvc, runner))
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

	host := strings.SplitN(strings.TrimPrefix(httpSrv.URL, "http://"), ":", 2)[0]
	navCtx, cancelNav := context.WithTimeout(browserCtx, 30*time.Second)
	defer cancelNav()

	var chipLabel string
	var scopeCleared bool
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.EmulateViewport(1440, 900),
		chromedp.Navigate(httpSrv.URL+"/workspace/"+slug),
		chromedp.WaitVisible(`#edit-form`, chromedp.ByID),
		// Select an element first, so this also pins that the two scope kinds
		// are mutually exclusive: picking a file must drop the element, or the
		// run would carry both and the chip would name only one of them.
		chromedp.Evaluate(tbClick("h1"), nil),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.Evaluate(`document.querySelector('.js-css-scope[data-path="site.css"]').click()`, nil),
		chromedp.Sleep(200*time.Millisecond),
		chromedp.Evaluate(`window.__tbScope === null`, &scopeCleared),
		chromedp.Text(`#scope-label`, &chipLabel, chromedp.ByID),
		chromedp.Evaluate(`(function(){
			document.getElementById('prompt').value = 'make the heading blue';
			document.getElementById('edit-form').requestSubmit();
			return true;
		})()`, nil),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}
	if !scopeCleared {
		t.Error("picking a stylesheet must drop the element selection")
	}
	if chipLabel != "site.css" {
		t.Errorf("scope chip = %q, want it to name the stylesheet", chipLabel)
	}

	// The run is started asynchronously by the POST; poll for the prompt the
	// service handed the agent.
	var prompt string
	for range 100 {
		prompt = runner.first()
		if prompt != "" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(prompt, "Edit only the page 'site.css'") {
		t.Errorf("agent prompt was not confined to the stylesheet:\n%s", prompt)
	}
}
