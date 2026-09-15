package linkcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jtarchie/topbanana/internal/storetest"
)

func fakeWeb(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/ok":
		case "/gone":
			w.WriteHeader(http.StatusNotFound)
		case "/nohead":
			if r.Method == http.MethodHead {
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case "/forbidden":
			w.WriteHeader(http.StatusForbidden)
		case "/boom":
			w.WriteHeader(http.StatusServiceUnavailable)
		case "/redirect":
			http.Redirect(w, r, "/ok", http.StatusFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func seed(t *testing.T, pages map[string]string) (*Checker, string) {
	t.Helper()
	s := storetest.New(t, 0)
	slug := storetest.FreshSlug(t, "linkcheck")
	for name, body := range pages {
		err := s.Write(context.Background(), slug, name, body, "text/html", nil)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return newChecker(s, nil), slug
}

func TestLinks(t *testing.T) {
	t.Parallel()

	c, slug := seed(t, map[string]string{
		"index.html": `<a href="https://ex.com/a#top">a</a><a href="https://ex.com/a">again</a>
<img src="//cdn.ex.com/i.png"><link rel="preconnect" href="https://fonts.ex.com">
<a href="about.html">internal</a><a href="mailto:x@ex.com">mail</a><form action="https://forms.ex.com/post"></form>`,
		"about.html": `<a href="https://ex.com/a">a</a><iframe src="HTTPS://maps.ex.com/embed"></iframe>`,
	})
	got, err := Links(context.Background(), c.store, slug)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"https://ex.com/a":          {"about.html", "index.html"},
		"https://cdn.ex.com/i.png":  {"index.html"},
		"https://maps.ex.com/embed": {"about.html"},
	}
	if len(got) != len(want) {
		t.Fatalf("Links = %v, want %v", got, want)
	}
	for u, pages := range want {
		slices.Sort(got[u])
		if !slices.Equal(got[u], pages) {
			t.Errorf("Links[%q] = %v, want %v", u, got[u], pages)
		}
	}
}

func TestCheck_ClassifiesAndCaches(t *testing.T) {
	t.Parallel()

	srv, hits := fakeWeb(t)
	var page strings.Builder
	for _, p := range []string{"ok", "gone", "nohead", "forbidden", "boom", "redirect"} {
		fmt.Fprintf(&page, `<a href="%s/%s">%s</a>`, srv.URL, p, p)
	}
	c, slug := seed(t, map[string]string{"index.html": page.String()})

	cached, err := c.Cached(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if !cached.Stale || cached.Count(StatusUnchecked) != 6 || hits.Load() != 0 {
		t.Fatalf("Cached before any check must be all-unchecked, stale, and offline: %+v hits=%d", cached, hits.Load())
	}

	rep, err := c.Check(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Status{
		"/ok": StatusOK, "/gone": StatusDead, "/nohead": StatusOK,
		"/forbidden": StatusUnverified, "/boom": StatusSuspect, "/redirect": StatusOK,
	}
	for _, r := range rep.Results {
		u, _ := url.Parse(r.URL)
		if r.Status != want[u.Path] {
			t.Errorf("%s: status %s (%s), want %s", u.Path, r.Status, r.Reason, want[u.Path])
		}
		if !slices.Equal(r.Pages, []string{"index.html"}) {
			t.Errorf("%s: pages %v", u.Path, r.Pages)
		}
	}
	if rep.Results[0].Status != StatusDead {
		t.Errorf("results must sort worst first, got %s first", rep.Results[0].Status)
	}
	if rep.Stale {
		t.Error("a completed check must not report stale")
	}

	before := hits.Load()
	again, err := c.Check(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if hits.Load() != before {
		t.Errorf("fresh cache must answer a re-check without the network: %d new hits", hits.Load()-before)
	}
	if again.Count(StatusDead) != 1 {
		t.Errorf("cached re-check lost the dead link: %+v", again.Results)
	}
}

// The production guard must refuse httptest's loopback address, and name why.
func TestCheck_GuardRefusesPrivateAddress(t *testing.T) {
	t.Parallel()

	srv, hits := fakeWeb(t)
	c, slug := seed(t, map[string]string{"index.html": `<a href="` + srv.URL + `/ok">x</a>`})
	guarded := New(c.store)

	rep, err := guarded.Check(context.Background(), slug)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Results) != 1 || rep.Results[0].Status != StatusUnverified || hits.Load() != 0 {
		t.Fatalf("loopback link must be unverified (never dead) without a request reaching it: %+v hits=%d", rep.Results, hits.Load())
	}
}

func TestClassify_NXDomainIsDead(t *testing.T) {
	t.Parallel()

	err := &url.Error{Op: "Head", URL: "https://nope.invalid", Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "nope.invalid", IsNotFound: true}}}
	if status, _ := classify(0, err); status != StatusDead {
		t.Errorf("NXDOMAIN: got %s, want dead", status)
	}
	if !certain(err) {
		t.Error("NXDOMAIN must skip the GET retry")
	}
	if status, _ := classify(0, errors.New("reset")); status != StatusSuspect {
		t.Errorf("unknown error: got %s, want suspect", status)
	}
}
