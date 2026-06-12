package lint

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// This file owns head-tag hygiene: the per-page basics every page needs for
// browsers to render, label, and index it correctly (charset, lang, title)
// and the cross-page rule that titles must be unique. These are the breakages
// a non-technical owner never notices directly — they just see garbled
// quotes, identical browser tabs, or a bare URL in search results.

// headFacts is what one pass over a page's elements reveals about its head.
type headFacts struct {
	charset     bool
	lang        bool
	hasTitle    bool
	title       string // first <title>'s trimmed text
	description bool
}

func collectHeadFacts(pi pageInfo) headFacts {
	var f headFacts
	for _, n := range pi.elements {
		switch n.Data {
		case "html":
			for _, a := range n.Attr {
				if a.Key == "lang" && strings.TrimSpace(a.Val) != "" {
					f.lang = true
				}
			}
		case "meta":
			if metaDeclaresCharset(n) {
				f.charset = true
			}
			if metaIsDescription(n) {
				f.description = true
			}
		case "title":
			if !f.hasTitle {
				f.hasTitle = true
				f.title = strings.TrimSpace(textContent(n))
			}
		}
	}
	return f
}

// metaDeclaresCharset reports whether a <meta> element declares the document
// encoding — either the modern <meta charset="..."> or the legacy
// <meta http-equiv="content-type" content="...; charset=...">.
func metaDeclaresCharset(n *html.Node) bool {
	var httpEquiv, content, charset string
	for _, a := range n.Attr {
		switch a.Key {
		case "charset":
			charset = strings.TrimSpace(a.Val)
		case "http-equiv":
			httpEquiv = strings.TrimSpace(a.Val)
		case "content":
			content = a.Val
		}
	}
	if charset != "" {
		return true
	}
	return strings.EqualFold(httpEquiv, "content-type") &&
		strings.Contains(strings.ToLower(content), "charset=")
}

// metaIsDescription reports whether a <meta> element is a name="description"
// declaration with non-empty content.
func metaIsDescription(n *html.Node) bool {
	var name, content string
	for _, a := range n.Attr {
		switch a.Key {
		case "name":
			name = strings.TrimSpace(a.Val)
		case "content":
			content = a.Val
		}
	}
	return strings.EqualFold(name, "description") && strings.TrimSpace(content) != ""
}

// textContent concatenates every text node under n.
func textContent(n *html.Node) string {
	var b strings.Builder
	WalkDOM(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return b.String()
}

// checkHeadHygiene flags a page missing its charset declaration, <html lang>,
// or a non-empty <title>.
func checkHeadHygiene(pi pageInfo) []Error {
	f := collectHeadFacts(pi)
	var errs []Error
	if !f.charset {
		errs = append(errs, Error{
			File:    pi.name,
			Kind:    KindMissingCharset,
			Message: `missing character encoding — the page has no <meta charset> in <head>, so browsers guess the encoding and non-ASCII text (curly quotes, accents, emoji) can render as garbled mojibake. Add <meta charset="utf-8"> as the first element inside <head>.`,
		})
	}
	if !f.lang {
		errs = append(errs, Error{
			File:    pi.name,
			Kind:    KindMissingLang,
			Message: `missing language — the <html> tag has no lang attribute, so screen readers mispronounce the content and search engines can't classify it. Add lang to <html> set to the language the page is actually written in (lang="en", lang="es", lang="fr", …) — use the site's real content language, not a default.`,
		})
	}
	if !f.hasTitle || f.title == "" {
		errs = append(errs, Error{
			File:    pi.name,
			Kind:    KindMissingTitle,
			Message: `missing <title> — the page has no non-empty <title> in <head>, so browser tabs, bookmarks, and search results show a bare URL instead of a name. Add a short, specific title describing this page.`,
		})
	}
	if !f.description {
		errs = append(errs, Error{
			File:    pi.name,
			Kind:    KindMissingDescription,
			Message: `missing meta description — the page has no <meta name="description"> in <head>, so search results and link previews fall back to arbitrary page text. Add <meta name="description" content="..."> with one or two sentences (~150 characters) saying what this specific page offers.`,
		})
	}
	return errs
}

// checkDuplicateTitles flags pages whose <title> text (whitespace-normalized)
// is identical to another page's. One page per group is treated as canonical
// — index.html when present, else the lexicographically first — and every
// other page in the group gets an error, so the agent renames the copies and
// leaves the original alone. Pages with a missing or empty title are skipped;
// checkHeadHygiene already reports those.
func checkDuplicateTitles(pages []pageInfo) []Error {
	groups := map[string][]string{}
	for _, p := range pages {
		f := collectHeadFacts(p)
		if !f.hasTitle || f.title == "" {
			continue
		}
		norm := strings.Join(strings.Fields(f.title), " ")
		groups[norm] = append(groups[norm], p.name)
	}

	titles := make([]string, 0, len(groups))
	for t, names := range groups {
		if len(names) > 1 {
			titles = append(titles, t)
		}
	}
	sort.Strings(titles)

	var errs []Error
	for _, title := range titles {
		names := groups[title]
		sort.Slice(names, func(i, j int) bool {
			if (names[i] == "index.html") != (names[j] == "index.html") {
				return names[i] == "index.html"
			}
			return names[i] < names[j]
		})
		canonical := names[0]
		for _, name := range names[1:] {
			errs = append(errs, Error{
				File: name,
				Kind: KindDuplicateTitle,
				Message: fmt.Sprintf(
					`duplicate <title> — this page's title %q is identical to %s's. Identical titles make browser tabs, history, and search results indistinguishable. Keep a shared site name if you like, but make each page's title unique (e.g. "Menu — Luigi's" vs "Contact — Luigi's").`,
					title, canonical),
			})
		}
	}
	return errs
}

// charsetMetaTag is the canonical encoding declaration AutoFixCharset injects.
const charsetMetaTag = `<meta charset="utf-8">`

// AutoFixCharset injects the canonical charset meta right after the opening
// <head> tag when the page declares no encoding at all. Idempotent (per the
// same detection checkHeadHygiene uses), and like the other fixers it must
// not run on a file with a KindSuspiciousAttr error — the build loop checks
// that before calling in. Returns the new content and whether anything
// changed.
func AutoFixCharset(content string) (string, bool) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return content, false
	}
	found := false
	WalkDOM(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == "meta" && metaDeclaresCharset(n) {
			found = true
		}
	})
	if found {
		return content, false
	}
	return injectAfterHeadOpen(content, charsetMetaTag)
}
