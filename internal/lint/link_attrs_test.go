package lint

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/storetest"
)

func TestSrcsetURLs(t *testing.T) {
	t.Parallel()

	cases := map[string][]string{
		"a.png":                 {"a.png"},
		"a.png 1x, b.png 2x":    {"a.png", "b.png"},
		"a.png 480w,b.png 800w": {"a.png", "b.png"},
		"a.png,b.png":           {"a.png,b.png"}, // spec: a comma inside a URL token is part of it
		"a.png, b.png":          {"a.png", "b.png"},
		"data:image/png;base64,iVBO 1x, b.png 2x": {"data:image/png;base64,iVBO", "b.png"},
		"  ": nil,
	}
	for in, want := range cases {
		if got := srcsetURLs(in); !slices.Equal(got, want) {
			t.Errorf("srcsetURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCSSURLs(t *testing.T) {
	t.Parallel()

	css := `.a{background:url(img/a.png)} .b{background:URL( "b.png" )}
/* .c{background:url(commented.png)} */ .d{mask:url('d.svg#x')} .e{fill:url(#grad)}`
	want := []string{"img/a.png", "b.png", "d.svg#x", "#grad"}
	if got := cssURLs(css); !slices.Equal(got, want) {
		t.Errorf("cssURLs = %q, want %q", got, want)
	}
}

func TestRefreshURL(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"0; url=next.html":   "next.html",
		"5;URL='/next.html'": "/next.html",
		"3, url = x.html":    "x.html",
		"30":                 "",
		"0;https://ex.com/":  "https://ex.com/",
	}
	for in, want := range cases {
		if got := refreshURL(in); got != want {
			t.Errorf("refreshURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// Look-alikes (data: srcset, url(#frag), commented url(), existing asset) must stay silent.
func TestApp_ExtendedLinkSources(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	s := storetest.New(t, 0)
	slug := storetest.FreshSlug(t, "lintattrs")

	index := `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"><link rel="stylesheet" href="css/site.css"><title>Home</title><meta name="description" content="Home page."><meta http-equiv="refresh" content="30; url=gone-refresh.html">
<style>.hero{background:url(gone-style.png)} .ok{background:url(logo.png)}</style></head><body>
<img src="logo.png" alt="" srcset="logo.png 1x, gone-srcset.png 2x">
<img src="logo.png" alt="" srcset="data:image/png;base64,AAAA 1x, logo.png 2x">
<video src="logo.png" poster="gone-poster.png"></video>
<object data="gone-object.svg"></object>
<div style="background-image:url('gone-inline.png')"></div>
<svg><rect fill="url(#g)"></rect></svg>
</body></html>`
	css := `/* url(commented.png) */ .x{background:url(../logo.png)} .y{background:url(gone-css.png)}`

	for name, content := range map[string]string{"index.html": index, "css/site.css": css, "logo.png": "png"} {
		err := s.Write(ctx, slug, name, content, "text/plain", nil)
		if err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var got []string
	for _, e := range App(ctx, s, slug, nil) {
		if !strings.Contains(e.Message, "broken link") {
			t.Errorf("unexpected non-link error: %s", e.Error())
			continue
		}
		got = append(got, e.File+" "+strings.SplitN(e.Message, `"`, 3)[1])
	}
	want := []string{
		"css/site.css gone-css.png",
		"index.html gone-inline.png",
		"index.html gone-object.svg",
		"index.html gone-poster.png",
		"index.html gone-refresh.html",
		"index.html gone-srcset.png",
		"index.html gone-style.png",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("broken links:\n got  %q\n want %q", got, want)
	}
}
