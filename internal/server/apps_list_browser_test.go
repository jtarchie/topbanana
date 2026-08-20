package server_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"

	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
)

// appsListState is the shape of the DOM probe below: it counts the <li>s that
// are *direct children* of #apps-list and how many of them are real app rows
// (data-name set), plus the per-row dropdown menus. The regression this test
// guards against showed up as rows > realRows: the filter/sort script used a
// descendant `querySelectorAll('li')`, so it also matched the four <li>s inside
// each row's dropdown menu, and appendChild() during sort tore them out of
// their menus and stacked them at the top of the list as bare rows.
type appsListState struct {
	Rows      int      `json:"rows"`
	RealRows  int      `json:"realRows"`
	Hoisted   int      `json:"hoisted"`
	Menus     int      `json:"menus"`
	MenuItems int      `json:"menuItems"`
	Order     []string `json:"order"`
	Visible   []string `json:"visible"`
	Count     string   `json:"count"`
	Total     string   `json:"total"`
}

const appsListProbe = `(function(){
	var list = document.getElementById('apps-list');
	var countEl = document.getElementById('apps-count');
	var rows = Array.prototype.slice.call(list.querySelectorAll(':scope > li'));
	var appRows = rows.filter(function (li) { return !!li.dataset.name; });
	var menus = Array.prototype.slice.call(list.querySelectorAll('.dropdown-content'));
	return {
		rows: rows.length,
		realRows: appRows.length,
		// A direct child that carries no data-name but does carry the shape of
		// a row-menu entry escaped its <ul>. Checked by shape rather than by
		// "not an app row": a non-row child added later (a header, a "load
		// more") is legitimate, and the [data-name] scoping exists precisely
		// so the script leaves it alone.
		hoisted: rows.filter(function (li) {
			return !li.dataset.name && li.querySelector('a[href^="/manage/"], a[href^="/inbox/"], form.js-confirm');
		}).length,
		menus: menus.length,
		menuItems: menus.reduce(function (n, ul) { return n + ul.querySelectorAll(':scope > li').length; }, 0),
		order: appRows.map(function (li) { return li.dataset.name; }),
		visible: appRows.filter(function (li) { return !li.hidden; }).map(function (li) { return li.dataset.name; }),
		count: countEl.textContent.trim(),
		total: countEl.dataset.total
	};
})()`

// seedEditedAt backdates a site's "Edited" timestamp by writing an empty
// transcript at the key that encodes it. editrec.List reads the timestamp out
// of the key alone (it never decodes the body), and appsHandler's lastEditedFor
// takes the newest row — so this is the whole of what drives data-edited, the
// sort key the recency ordering runs on.
func seedEditedAt(t *testing.T, ctx context.Context, st *store.Store, slug string, at time.Time) {
	t.Helper()
	key := editrec.Key(slug, at, "seedlog")
	err := st.WriteRaw(ctx, key, "{}", "application/json", nil)
	if err != nil {
		t.Fatalf("seed transcript for %s: %v", slug, err)
	}
	t.Cleanup(func() { _ = editrec.Delete(context.WithoutCancel(ctx), st, key) })
}

// TestAppsList_ControlsKeepRowsIntactInBrowser drives the /apps filter + sort
// controls in a real browser. The controls only render past 8 apps, so this
// seeds 9 — below that threshold the script never initializes and the bug is
// invisible. Server-side rendering was always correct here; only a browser
// running the page's own JS can catch it.
//
// Skips when Chrome isn't installed.
func TestAppsList_ControlsKeepRowsIntactInBrowser(t *testing.T) {
	st := minioStore(t)
	chromePath := chromeExecPath(t)
	if chromePath == "" {
		t.Skip("no Chrome binary found — skipping browser test")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// Nine apps: one over the threshold that server-renders #apps-controls.
	// Each gets a distinct edit timestamp, an hour apart, newest first — so
	// the recency sort has a real ordering to get wrong. Without them every
	// row carries data-edited="0" and the comparator returns 0 for every pair,
	// which passes any ordering assertion you care to write.
	const seeded = 9
	editedAt := time.Now().UTC().Add(-24 * time.Hour)
	byRecency := make([]string, 0, seeded)
	for i := range seeded {
		slug := freshSlug(t)
		cleanupSlug(t, ctx, st, snapSvc, slug)
		mustWrite(t, ctx, st, slug, "index.html", "<h1>"+slug+"</h1>", "text/html")
		seedEditedAt(t, ctx, st, slug, editedAt.Add(-time.Duration(i)*time.Hour))
		byRecency = append(byRecency, slug)
	}
	byName := slices.Clone(byRecency)
	sort.Strings(byName)

	// Precondition, asserted over plain HTTP before the browser is involved:
	// the page must render all 9 rows *and* the >8 controls block. In the
	// browser this shows up only as a WaitVisible that never resolves, which
	// is indistinguishable from a slow machine.
	assertAppsPageRenders(t, httpSrv.URL, byRecency)

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

	var onLoad, afterSort, afterFilter appsListState
	err := chromedp.Run(navCtx,
		network.SetCookies([]*network.CookieParam{{
			Name:   testSessionCookie.Name,
			Value:  testSessionCookie.Value,
			Domain: host,
			Path:   "/",
		}}),
		chromedp.Navigate(httpSrv.URL+"/apps"),
		chromedp.WaitVisible(`#apps-controls`, chromedp.ByID),
		chromedp.Evaluate(appsListProbe, &onLoad),
		// Switching sort re-appends every matched <li> — the step that used to
		// move the dropdown items out of their menus.
		chromedp.Evaluate(`(function(){
			var sel = document.getElementById('apps-sort');
			sel.value = 'recent';
			sel.dispatchEvent(new Event('change'));
		})()`, nil),
		chromedp.Evaluate(appsListProbe, &afterSort),
		chromedp.SendKeys(`#apps-filter`, byRecency[0], chromedp.ByID),
		chromedp.Evaluate(appsListProbe, &afterFilter),
	)
	if err != nil {
		if shouldSkipChrome(err) {
			t.Skipf("chromedp run failed (%v) — skipping", err)
		}
		t.Fatalf("chromedp run: %v", err)
	}

	for _, stage := range []struct {
		name  string
		state appsListState
	}{
		{"on load", onLoad},
		{"after sort", afterSort},
		{"after filter", afterFilter},
	} {
		assertRowsIntact(t, stage.name, stage.state, onLoad, seeded)
	}
	if onLoad.MenuItems != onLoad.Menus*4 {
		t.Errorf("on load: %d menu items across %d menus — expected the 4-item row menu (Edit/Inbox/Settings/Delete); update this if the menu changed", onLoad.MenuItems, onLoad.Menus)
	}

	// A shared Minio bucket (the storetest conformance mode) can hold apps
	// from other tests, and the seeded session is a super admin so it sees
	// them all. Assert the relative order of the seeded slugs only.
	if got := onlySeeded(onLoad.Order, byRecency); !slices.Equal(got, byName) {
		t.Errorf("default (alpha) order = %v, want %v", got, byName)
	}
	if got := onlySeeded(afterSort.Order, byRecency); !slices.Equal(got, byRecency) {
		t.Errorf("recency order = %v, want newest-first %v", got, byRecency)
	}

	// The count reads off the same selector, so it renders "45 of 9" when the
	// selector over-matches.
	if onLoad.Count != onLoad.Total {
		t.Errorf("unfiltered count = %q, want the bare total %q", onLoad.Count, onLoad.Total)
	}
	if !slices.Contains(afterFilter.Visible, byRecency[0]) {
		t.Errorf("filtering on the full slug %q hid its own row (visible: %v)", byRecency[0], afterFilter.Visible)
	}
	for _, slug := range byRecency[1:] {
		if slices.Contains(afterFilter.Visible, slug) {
			t.Errorf("filtering on %q left non-matching row %q visible", byRecency[0], slug)
		}
	}
	if want := fmt.Sprintf("%d of %s", len(afterFilter.Visible), afterFilter.Total); afterFilter.Count != want {
		t.Errorf("filtered count = %q, want %q", afterFilter.Count, want)
	}
}

// assertRowsIntact checks the invariant the filter/sort script must preserve at
// every stage: no menu item escaped its <ul>, every seeded row is still a row,
// and each row still owns a complete menu. The menu-item count is compared
// against the as-rendered load-time count rather than a hardcoded
// items-per-row — what matters is that sorting and filtering leave the menus
// untouched, whatever the row menu happens to contain.
func assertRowsIntact(t *testing.T, stage string, got, onLoad appsListState, seeded int) {
	t.Helper()
	if got.Hoisted > 0 {
		t.Errorf("%s: %d dropdown menu items were hoisted out of their menus into #apps-list (%d direct <li> children, %d of them app rows)",
			stage, got.Hoisted, got.Rows, got.RealRows)
	}
	if got.RealRows < seeded {
		t.Errorf("%s: %d app rows, want at least the %d seeded", stage, got.RealRows, seeded)
	}
	if got.Menus != got.RealRows {
		t.Errorf("%s: %d dropdown menus for %d rows", stage, got.Menus, got.RealRows)
	}
	if got.MenuItems != onLoad.MenuItems {
		t.Errorf("%s: %d dropdown menu items, want the %d present on load", stage, got.MenuItems, onLoad.MenuItems)
	}
}

// onlySeeded drops rows this test didn't create, so assertions on ordering
// survive a shared bucket.
func onlySeeded(order, seeded []string) []string {
	out := make([]string, 0, len(seeded))
	for _, name := range order {
		if slices.Contains(seeded, name) {
			out = append(out, name)
		}
	}
	return out
}

// assertAppsPageRenders fails when /apps doesn't serve every seeded slug plus
// the filter/sort controls. The browser test is meaningless without both, and
// a missing precondition is far easier to read here than as a chromedp
// timeout 30 seconds later.
func assertAppsPageRenders(t *testing.T, baseURL string, slugs []string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, baseURL+"/apps", nil)
	if err != nil {
		t.Fatalf("build /apps request: %v", err)
	}
	req.AddCookie(testSessionCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /apps: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /apps: status %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /apps body: %v", err)
	}
	page := string(body)
	if !strings.Contains(page, `id="apps-controls"`) {
		t.Fatalf("/apps did not render #apps-controls — the filter/sort script never initializes, so this test would pass without exercising anything")
	}
	for _, slug := range slugs {
		if !strings.Contains(page, `data-name="`+slug+`"`) {
			t.Fatalf("/apps is missing seeded app %q — check the owner/role filter in appsHandler", slug)
		}
	}
}
