package lint

import (
	"strings"
	"testing"
)

// orphanSite runs checkUnreferencedPages over a map of page name → body
// (wrapped in a minimal document) plus optional function literals.
func orphanSite(t *testing.T, bodies map[string]string, fnLiterals []string, skeleton map[string]bool) []Error {
	t.Helper()
	fileSet := map[string]bool{}
	pages := make([]pageInfo, 0, len(bodies))
	factsByPage := map[string]jsFacts{}
	for name, body := range bodies {
		fileSet[name] = true
		pi := pageOf(t, name, `<!DOCTYPE html><html><body>`+body+`</body></html>`)
		pages = append(pages, pi)
		factsByPage[name] = collectJSFacts(name, pi.scripts)
	}
	return checkUnreferencedPages(pages, factsByPage, fnLiterals, skeleton, linkCheckContext{fileSet: fileSet})
}

func TestCheckUnreferencedPages(t *testing.T) {
	t.Parallel()

	t.Run("linked page passes", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{
			"index.html": `<a href="about.html">about</a>`,
			"about.html": `<p>hi</p>`,
		}, nil, nil)
		if len(errs) != 0 {
			t.Fatalf("linked page flagged: %+v", errs)
		}
	})

	t.Run("unreferenced page flags", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{
			"index.html":  `<p>home</p>`,
			"extras.html": `<p>lost</p>`,
		}, nil, nil)
		if len(errs) != 1 || errs[0].File != "extras.html" || errs[0].Kind != KindUnreferencedPage {
			t.Fatalf("expected extras.html flagged, got %+v", errs)
		}
		if !strings.Contains(errs[0].Message, "unreachable page") {
			t.Errorf("message: %s", errs[0].Message)
		}
	})

	t.Run("index.html never flags", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{"index.html": `<p>alone</p>`}, nil, nil)
		if len(errs) != 0 {
			t.Fatalf("index.html must never be an orphan: %+v", errs)
		}
	})

	t.Run("self-reference does not count", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{
			"index.html":  `<p>home</p>`,
			"extras.html": `<a href="extras.html">me</a>`,
		}, nil, nil)
		if len(errs) != 1 || errs[0].File != "extras.html" {
			t.Fatalf("a page whose only mention is itself is still unreachable: %+v", errs)
		}
	})

	t.Run("inline-script navigation counts", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{
			"index.html":  `<script>location.href = 'thanks.html';</script>`,
			"thanks.html": `<p>thanks</p>`,
		}, nil, nil)
		if len(errs) != 0 {
			t.Fatalf("script-referenced page flagged: %+v", errs)
		}
	})

	t.Run("functions redirect literal counts", func(t *testing.T) {
		t.Parallel()
		// The contact-form shape: thanks.html is reachable only through
		// response.redirect("/thanks.html") in functions/submit.js.
		errs := orphanSite(t, map[string]string{
			"index.html":  `<form action="/api/submit"><input name="email"></form>`,
			"thanks.html": `<p>thanks</p>`,
		}, []string{"/thanks.html"}, nil)
		if len(errs) != 0 {
			t.Fatalf("function-redirect-referenced page flagged: %+v", errs)
		}
	})

	t.Run("skeleton pages are exempt", func(t *testing.T) {
		t.Parallel()
		// The tiny-shop shape: orders.html is the owner-facing order log,
		// deliberately unlinked from customer pages.
		errs := orphanSite(t, map[string]string{
			"index.html":  `<p>shop</p>`,
			"orders.html": `<p>order log</p>`,
		}, nil, map[string]bool{"orders.html": true})
		if len(errs) != 0 {
			t.Fatalf("skeleton-shipped page flagged: %+v", errs)
		}
	})

	t.Run("subdirectory page reached via directory link", func(t *testing.T) {
		t.Parallel()
		errs := orphanSite(t, map[string]string{
			"index.html":      `<a href="blog/">blog</a>`,
			"blog/index.html": `<p>posts</p>`,
		}, nil, nil)
		if len(errs) != 0 {
			t.Fatalf("directory-linked index flagged: %+v", errs)
		}
	})
}
