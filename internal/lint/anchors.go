package lint

import (
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

// This file owns anchor-fragment validation: every `#fragment` in an href
// must name a real id on the page the link resolves to. The page-link check
// (checkLink) deliberately strips fragments before resolving, so without
// this check a nav full of `#section` links to ids that were never written
// lints clean and silently does nothing in the browser.

// parsedPage pairs an HTML file's name with its parsed DOM. App collects one
// per page so cross-page checks like anchors can see the whole site at once.
type parsedPage struct {
	name string
	doc  *html.Node
}

// anchorTargets collects the fragment targets one page exposes: the id
// attribute of every element (which also covers inline-SVG <symbol> ids
// referenced by `<use href="#...">`) plus the legacy name attribute on <a>.
func anchorTargets(doc *html.Node) map[string]bool {
	targets := map[string]bool{}
	WalkDOM(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		for _, attr := range n.Attr {
			switch {
			case attr.Key == "id" && attr.Val != "":
				targets[attr.Val] = true
			case attr.Key == "name" && n.Data == "a" && attr.Val != "":
				targets[attr.Val] = true
			}
		}
	})
	return targets
}

// checkAnchors validates every href fragment on every page: `#x` must match
// an id on the same page, `page.html#x` an id on the page the link resolves
// to (same resolution as the broken-link check, fallbacks included). Each
// distinct broken href is reported once per page — the same value repeated
// in a navbar and a footer is one mistake, not several.
func checkAnchors(pages []parsedPage, lc linkCheckContext) []Error {
	targetsByPage := make(map[string]map[string]bool, len(pages))
	for _, p := range pages {
		targetsByPage[p.name] = anchorTargets(p.doc)
	}

	var errs []Error
	for _, p := range pages {
		dir := path.Dir(p.name)
		seen := map[string]bool{}
		WalkDOM(p.doc, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			for _, attr := range n.Attr {
				if attr.Key != "href" {
					continue
				}
				href := strings.TrimSpace(attr.Val)
				if seen[href] {
					continue
				}
				seen[href] = true
				e := checkAnchorHref(p.name, dir, href, targetsByPage, lc)
				if e != nil {
					errs = append(errs, *e)
				}
			}
		})
	}
	return errs
}

// checkAnchorHref validates one href's fragment. Returns nil when the href
// carries no fragment, the fragment is exempt, the target page itself is
// missing or not HTML (checkLink's territory — a second error for the same
// href would be noise), or the fragment matches an id on the target page.
func checkAnchorHref(filename, dir, href string, targetsByPage map[string]map[string]bool, lc linkCheckContext) *Error {
	if href == "" || IsExternalLink(href) {
		return nil
	}
	hash := strings.IndexByte(href, '#')
	if hash == -1 {
		return nil
	}
	pagePart, frag := href[:hash], href[hash+1:]
	if frag == "" {
		// `#` and `page.html#` scroll to the top of the target — always valid.
		return nil
	}
	if strings.EqualFold(frag, "top") {
		// The HTML spec falls back to the top of the document for `#top`
		// even when no element carries that id.
		return nil
	}
	dec, err := url.PathUnescape(frag)
	if err == nil {
		frag = dec
	}
	if i := strings.IndexByte(pagePart, '?'); i != -1 {
		pagePart = pagePart[:i]
	}

	target := filename
	if pagePart != "" {
		resolved, ok := resolveLinkTarget(dir, pagePart, lc.fileSet)
		if !ok {
			return nil
		}
		target = resolved
	}
	ids, ok := targetsByPage[target]
	if !ok {
		// Fragment on a non-HTML file (an asset) — nothing to validate.
		return nil
	}
	if ids[frag] {
		return nil
	}
	return &Error{
		File:    filename,
		Kind:    KindBrokenAnchor,
		Message: brokenAnchorMessage(href, frag, target, filename, ids),
	}
}

// brokenAnchorMessage words a broken-anchor error for the agent: the bad
// href, where the id is missing, the ids that do exist there (capped), and
// both repair options — the agent decides which one the page intended.
func brokenAnchorMessage(href, frag, target, filename string, ids map[string]bool) string {
	where := "this page"
	if target != filename {
		where = target
	}
	var b strings.Builder
	fmt.Fprintf(&b, "broken anchor %q — %s has no element with id=%q.", href, where, frag)
	if len(ids) == 0 {
		b.WriteString(" That page has no element ids at all.")
	} else {
		sorted := make([]string, 0, len(ids))
		for id := range ids {
			sorted = append(sorted, id)
		}
		sort.Strings(sorted)
		fmt.Fprintf(&b, " Existing ids there: %s.", capList(sorted, maxListedItems))
	}
	fmt.Fprintf(&b, " Add id=%q to the element this link should scroll to, or change the fragment to an existing id.", frag)
	return b.String()
}
