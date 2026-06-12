package lint

import (
	"strings"
	"testing"
)

func TestCheckHeadHygiene(t *testing.T) {
	t.Parallel()

	const fullHead = `<meta charset="utf-8"><title>Home</title><meta name="description" content="What this page offers.">`

	cases := []struct {
		name      string
		page      string
		wantKinds []Kind
	}{
		{
			name:      "complete head passes",
			page:      `<!DOCTYPE html><html lang="en"><head>` + fullHead + `</head><body></body></html>`,
			wantKinds: nil,
		},
		{
			name:      "legacy http-equiv charset passes",
			page:      `<!DOCTYPE html><html lang="en"><head><meta http-equiv="Content-Type" content="text/html; charset=UTF-8"><title>Home</title><meta name="description" content="d"></head><body></body></html>`,
			wantKinds: nil,
		},
		{
			name:      "missing charset",
			page:      `<!DOCTYPE html><html lang="en"><head><title>Home</title><meta name="description" content="d"></head><body></body></html>`,
			wantKinds: []Kind{KindMissingCharset},
		},
		{
			name:      "missing description",
			page:      `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><title>Home</title></head><body></body></html>`,
			wantKinds: []Kind{KindMissingDescription},
		},
		{
			name:      "empty description content is missing",
			page:      `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><title>Home</title><meta name="description" content="  "></head><body></body></html>`,
			wantKinds: []Kind{KindMissingDescription},
		},
		{
			name:      "missing lang",
			page:      `<!DOCTYPE html><html><head>` + fullHead + `</head><body></body></html>`,
			wantKinds: []Kind{KindMissingLang},
		},
		{
			name:      "whitespace lang is missing",
			page:      `<!DOCTYPE html><html lang="  "><head>` + fullHead + `</head><body></body></html>`,
			wantKinds: []Kind{KindMissingLang},
		},
		{
			name:      "missing title",
			page:      `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="description" content="d"></head><body></body></html>`,
			wantKinds: []Kind{KindMissingTitle},
		},
		{
			name:      "empty title is missing",
			page:      `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><meta name="description" content="d"><title>   </title></head><body></body></html>`,
			wantKinds: []Kind{KindMissingTitle},
		},
		{
			name:      "bare page reports all four",
			page:      `<html><head></head><body></body></html>`,
			wantKinds: []Kind{KindMissingCharset, KindMissingLang, KindMissingTitle, KindMissingDescription},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			errs := checkHeadHygiene(pageOf(t, "index.html", tc.page))
			if len(errs) != len(tc.wantKinds) {
				t.Fatalf("got %d errors %+v, want kinds %v", len(errs), errs, tc.wantKinds)
			}
			for i, want := range tc.wantKinds {
				if errs[i].Kind != want {
					t.Errorf("errs[%d].Kind = %q, want %q", i, errs[i].Kind, want)
				}
			}
		})
	}
}

func TestCheckDuplicateTitles(t *testing.T) {
	t.Parallel()

	page := func(t *testing.T, name, title string) pageInfo {
		t.Helper()
		return pageOf(t, name, `<!DOCTYPE html><html lang="en"><head><meta charset="utf-8"><title>`+title+`</title></head><body></body></html>`)
	}

	t.Run("unique titles pass", func(t *testing.T) {
		t.Parallel()
		errs := checkDuplicateTitles([]pageInfo{
			page(t, "index.html", "Home"),
			page(t, "about.html", "About"),
		})
		if len(errs) != 0 {
			t.Fatalf("unique titles flagged: %+v", errs)
		}
	})

	t.Run("index.html is canonical, others flagged", func(t *testing.T) {
		t.Parallel()
		errs := checkDuplicateTitles([]pageInfo{
			page(t, "about.html", "Shop"),
			page(t, "index.html", "Shop"),
			page(t, "contact.html", "  Shop  "), // whitespace-normalized match
		})
		if len(errs) != 2 {
			t.Fatalf("expected 2 errors (index.html canonical), got %+v", errs)
		}
		for _, e := range errs {
			if e.File == "index.html" {
				t.Errorf("index.html must be the canonical keeper, not flagged: %+v", e)
			}
			if e.Kind != KindDuplicateTitle {
				t.Errorf("Kind = %q, want %q", e.Kind, KindDuplicateTitle)
			}
			if !strings.Contains(e.Message, `"Shop"`) || !strings.Contains(e.Message, "index.html") {
				t.Errorf("message must name the title and the canonical page: %s", e.Message)
			}
		}
	})

	t.Run("empty titles are not grouped", func(t *testing.T) {
		t.Parallel()
		errs := checkDuplicateTitles([]pageInfo{
			pageOf(t, "a.html", `<html><head></head><body></body></html>`),
			pageOf(t, "b.html", `<html><head></head><body></body></html>`),
		})
		if len(errs) != 0 {
			t.Fatalf("pages without titles must be checkHeadHygiene's problem, got %+v", errs)
		}
	})
}

func TestAutoFixCharset(t *testing.T) {
	t.Parallel()

	t.Run("injects right after head open", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><style>body{}</style><title>x</title></head><body></body></html>`
		out, changed := AutoFixCharset(in)
		if !changed {
			t.Fatal("expected changed=true")
		}
		headIdx := strings.Index(out, "<head>")
		charsetIdx := strings.Index(out, charsetMetaTag)
		styleIdx := strings.Index(out, "<style>")
		if charsetIdx == -1 || charsetIdx < headIdx || charsetIdx > styleIdx {
			t.Errorf("charset must land immediately inside <head>, before other children:\n%s", out)
		}
	})

	t.Run("idempotent when present", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><meta charset="utf-8"><title>x</title></head><body></body></html>`
		out, changed := AutoFixCharset(in)
		if changed {
			t.Fatalf("expected changed=false:\n%s", out)
		}
	})

	t.Run("http-equiv counts as present", func(t *testing.T) {
		in := `<!DOCTYPE html><html><head><meta http-equiv="content-type" content="text/html; charset=utf-8"></head><body></body></html>`
		if _, changed := AutoFixCharset(in); changed {
			t.Fatal("legacy declaration must satisfy the fixer")
		}
	})

	t.Run("no head open tag returns unchanged", func(t *testing.T) {
		in := `<body><p>hi</p></body>`
		out, changed := AutoFixCharset(in)
		if changed || out != in {
			t.Errorf("expected unchanged content, got changed=%v:\n%s", changed, out)
		}
	})

	t.Run("header element is not head", func(t *testing.T) {
		in := `<header>x</header>`
		if out, changed := injectAfterHeadOpen(in, charsetMetaTag); changed {
			t.Errorf("<header> must not match <head>: %s", out)
		}
	})

	t.Run("head with attributes", func(t *testing.T) {
		in := `<html><head data-x="1"><title>x</title></head></html>`
		out, changed := injectAfterHeadOpen(in, charsetMetaTag)
		if !changed || !strings.Contains(out, `<head data-x="1">`+"\n"+charsetMetaTag) {
			t.Errorf("must inject after the full open tag: %s", out)
		}
	})
}
