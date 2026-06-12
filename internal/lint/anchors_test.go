package lint

import (
	"context"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/storetest"
)

func mustParse(t *testing.T, src string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse: %v", err)
	}
	return doc
}

// pageOf parses src and collects its pageInfo — the shape every cross-page
// check consumes.
func pageOf(t *testing.T, name, src string) pageInfo {
	t.Helper()
	return collectPageInfo(name, mustParse(t, src))
}

func TestAnchorTargets(t *testing.T) {
	t.Parallel()

	doc := mustParse(t, `<!DOCTYPE html><html><body>
<section id="hero"></section>
<a name="legacy">old-school target</a>
<div name="not-an-anchor"></div>
<span id="">empty id ignored</span>
<svg><symbol id="icon-star"></symbol><use href="#icon-star"></use></svg>
</body></html>`)

	got := collectPageInfo("index.html", doc).targets
	for _, want := range []string{"hero", "legacy", "icon-star"} {
		if !got[want] {
			t.Errorf("anchorTargets missing %q: %v", want, got)
		}
	}
	if got["not-an-anchor"] {
		t.Error("name attribute on a non-<a> element must not be an anchor target")
	}
	if got[""] {
		t.Error("empty id must not be an anchor target")
	}
}

//nolint:gochecknoglobals // shared single-page fileSet for the table tests below.
var onePageSet = map[string]bool{"index.html": true}

func TestCheckAnchors_SamePage(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"fragment with matching id passes", `<section id="features"></section><a href="#features">x</a>`, false},
		{"fragment without matching id fails", `<a href="#pricing">x</a>`, true},
		{"bare # passes", `<a href="#">x</a>`, false},
		{"#top passes without an id (spec fallback)", `<a href="#top">x</a>`, false},
		{"external fragment skipped", `<a href="https://example.com/docs#install">x</a>`, false},
		{"url-encoded fragment matches decoded id", `<section id="café"></section><a href="#caf%C3%A9">x</a>`, false},
		{"svg use fragment resolves to symbol id", `<svg><symbol id="icon-star"></symbol><use href="#icon-star"></use></svg>`, false},
		{"query before fragment still targets this page", `<section id="results"></section><a href="?q=1#results">x</a>`, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			pages := []pageInfo{pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+tc.body+`</body></html>`)}
			errs := checkAnchors(pages, linkCheckContext{fileSet: onePageSet})
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkAnchors(%q) = nil, want error", tc.body)
			}
			if !tc.wantErr && len(errs) > 0 {
				t.Fatalf("checkAnchors(%q) = %+v, want nil", tc.body, errs)
			}
			for _, e := range errs {
				if e.Kind != KindBrokenAnchor {
					t.Errorf("expected KindBrokenAnchor, got %q", e.Kind)
				}
			}
		})
	}
}

func TestCheckAnchors_SamePage_MessageIsActionable(t *testing.T) {
	t.Parallel()

	pages := []pageInfo{pageOf(t, "index.html", `<!DOCTYPE html><html><body>
<section id="hero"></section><section id="contact"></section>
<a href="#pricing">x</a>
</body></html>`)}
	errs := checkAnchors(pages, linkCheckContext{fileSet: onePageSet})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	msg := errs[0].Message
	for _, want := range []string{`"#pricing"`, `id="pricing"`, "contact, hero", `Add id="pricing"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestCheckAnchors_DedupesRepeatedHref(t *testing.T) {
	t.Parallel()

	// The same broken href in the navbar and the footer is one mistake.
	pages := []pageInfo{pageOf(t, "index.html", `<!DOCTYPE html><html><body>
<nav><a href="#missing">x</a></nav>
<footer><a href="#missing">x</a></footer>
</body></html>`)}
	errs := checkAnchors(pages, linkCheckContext{fileSet: onePageSet})
	if len(errs) != 1 {
		t.Fatalf("expected exactly 1 deduped error, got %d: %+v", len(errs), errs)
	}
}

func TestCheckAnchors_CrossPage(t *testing.T) {
	t.Parallel()

	index := mustParse(t, `<!DOCTYPE html><html><body><section id="hero"></section><a name="legacy">x</a></body></html>`)
	about := mustParse(t, `<!DOCTYPE html><html><body><section id="team"></section></body></html>`)
	guide := mustParse(t, `<!DOCTYPE html><html><body></body></html>`)
	fileSet := map[string]bool{
		"index.html":      true,
		"about.html":      true,
		"docs/guide.html": true,
		"logo.png":        true,
	}

	cases := []struct {
		name    string
		from    string
		href    string
		wantErr bool
	}{
		{"cross-page fragment with matching id passes", "index.html", "about.html#team", false},
		{"cross-page fragment without matching id fails", "index.html", "about.html#ghost", true},
		{"extensionless target resolves like the proxy", "index.html", "about#team", false},
		{"missing target page is checkLink's territory", "index.html", "missing.html#team", false},
		{"fragment on a non-HTML asset is skipped", "index.html", "logo.png#frag", false},
		{"legacy a-name target passes", "about.html", "index.html#legacy", false},
		{"subdir page reaches root id via ..", "docs/guide.html", "../index.html#hero", false},
		{"subdir page reaches root id via absolute path", "docs/guide.html", "/index.html#hero", false},
		{"subdir page fails on missing root id", "docs/guide.html", "/index.html#nope", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			docs := map[string]*html.Node{"index.html": index, "about.html": about, "docs/guide.html": guide}
			src := `<!DOCTYPE html><html><body><a href="` + tc.href + `">x</a></body></html>`
			docs[tc.from] = mustParse(t, src)
			pages := make([]pageInfo, 0, len(docs))
			for name, doc := range docs {
				pages = append(pages, collectPageInfo(name, doc))
			}
			errs := checkAnchors(pages, linkCheckContext{fileSet: fileSet})
			if tc.wantErr && len(errs) == 0 {
				t.Fatalf("checkAnchors(%s -> %q) = nil, want error", tc.from, tc.href)
			}
			if !tc.wantErr && len(errs) > 0 {
				t.Fatalf("checkAnchors(%s -> %q) = %+v, want nil", tc.from, tc.href, errs)
			}
		})
	}
}

func TestCheckAnchors_CrossPageMessageNamesTargetPage(t *testing.T) {
	t.Parallel()

	pages := []pageInfo{
		pageOf(t, "index.html", `<!DOCTYPE html><html><body><a href="about.html#ghost">x</a></body></html>`),
		pageOf(t, "about.html", `<!DOCTYPE html><html><body><section id="team"></section></body></html>`),
	}
	errs := checkAnchors(pages, linkCheckContext{fileSet: map[string]bool{"index.html": true, "about.html": true}})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	if errs[0].File != "index.html" {
		t.Errorf("error must be attributed to the linking page, got %q", errs[0].File)
	}
	msg := errs[0].Message
	for _, want := range []string{"about.html", `id="ghost"`, "team"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q:\n%s", want, msg)
		}
	}
}

func TestCheckAnchors_IDListIsCapped(t *testing.T) {
	t.Parallel()

	var body strings.Builder
	for i := range 20 {
		body.WriteString(`<section id="s` + string(rune('a'+i)) + `"></section>`)
	}
	body.WriteString(`<a href="#missing">x</a>`)
	pages := []pageInfo{pageOf(t, "index.html", `<!DOCTYPE html><html><body>`+body.String()+`</body></html>`)}
	errs := checkAnchors(pages, linkCheckContext{fileSet: onePageSet})
	if len(errs) != 1 {
		t.Fatalf("expected 1 error, got %+v", errs)
	}
	if !strings.Contains(errs[0].Message, "+5 more") {
		t.Errorf("expected the id list capped at %d with a +5 more suffix:\n%s", maxListedItems, errs[0].Message)
	}
}

// TestApp_LinkAndAnchorChecks exercises the whole App pass over a real store:
// a well-formed two-page site with one broken anchor and one typo'd page link
// must produce exactly those two errors, with the anchor error carrying its
// kind and the link error carrying the did-you-mean suggestion.
func TestApp_LinkAndAnchorChecks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := storetest.New(t, 0)
	slug := storetest.FreshSlug(t, "lintanchor")

	head := `<head><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"><title>x</title></head>`
	index := `<!DOCTYPE html><html>` + head + `<body>
<a href="abuot.html">typo link</a>
<a href="about.html#ghost">broken anchor</a>
<a href="about.html#team">fine</a>
</body></html>`
	about := `<!DOCTYPE html><html>` + head + `<body><section id="team"></section></body></html>`

	for name, content := range map[string]string{"index.html": index, "about.html": about} {
		err := s.Write(ctx, slug, name, content, "text/html", nil)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	errs := App(ctx, s, slug, nil)
	if len(errs) != 2 {
		t.Fatalf("expected 2 errors (broken link + broken anchor), got %d: %+v", len(errs), errs)
	}

	var sawLink, sawAnchor bool
	for _, e := range errs {
		switch {
		case strings.Contains(e.Message, "broken link"):
			sawLink = true
			if !strings.Contains(e.Message, `Did you mean "about.html"?`) {
				t.Errorf("broken-link message missing did-you-mean:\n%s", e.Message)
			}
		case e.Kind == KindBrokenAnchor:
			sawAnchor = true
			if !strings.Contains(e.Message, `id="ghost"`) {
				t.Errorf("broken-anchor message missing the fragment:\n%s", e.Message)
			}
		}
	}
	if !sawLink || !sawAnchor {
		t.Errorf("expected one broken-link and one broken-anchor error, got %+v", errs)
	}
}
