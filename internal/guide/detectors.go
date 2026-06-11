package guide

import (
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/templates"
)

// parsedPage is one HTML page parsed once and reused across every detector.
type parsedPage struct {
	Path string
	Doc  *html.Node
}

// detector reports whether the feature is present within the given page set.
// The evaluator decides which pages to pass (see Scope), so detectors stay
// scope-agnostic: "is it present in at least one of these pages" for presence
// checks, or "does the total across these pages meet the minimum" for counts.
type detector func(p templates.GuideParams, pages []parsedPage) bool

// detectors is the closed registry. Template authors reference these keys in
// prompt.md frontmatter; the matching logic lives here in Go.
var detectors = map[string]detector{
	"tel_link":        detectTelLink,
	"email_link":      detectEmailLink,
	"form":            detectForm,
	"heading_matches": detectHeadingMatches,
	"section_present": detectSectionPresent,
	"address":         detectAddress,
	"map_link":        detectMapLink,
	"min_images":      detectMinImages,
	"min_links":       detectMinLinks,
}

// KnownDetectors returns the set of valid detector keys, so the templates test
// can assert every shipped guide item references a real detector.
func KnownDetectors() map[string]bool {
	out := make(map[string]bool, len(detectors))
	for k := range detectors {
		out[k] = true
	}
	return out
}

// sectionMinBodyChars is the non-whitespace body-text threshold below which a
// matched section heading is treated as an empty placeholder (heading present
// but nothing real under it). The defense against "Hours" headings with no
// hours beneath them.
const sectionMinBodyChars = 25

func detectTelLink(_ templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool { return hasLinkPrefix(doc, "tel:") })
}

func detectEmailLink(_ templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool { return hasLinkPrefix(doc, "mailto:") })
}

func detectForm(_ templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool { return hasElement(doc, "form") })
}

func detectHeadingMatches(p templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool { return headingMatch(doc, p.Keywords) })
}

func detectSectionPresent(p templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool { return sectionPresent(doc, p.Keywords) })
}

// addressKeywords lets detectAddress fall back to a location-style heading when
// there's no semantic <address> element.
var addressKeywords = []string{"location", "address", "directions", "find us", "visit", "where"}

func detectAddress(_ templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, func(doc *html.Node) bool {
		return hasNonEmptyElement(doc, "address") || headingMatch(doc, addressKeywords)
	})
}

func detectMapLink(_ templates.GuideParams, pages []parsedPage) bool {
	return anyPage(pages, hasMapLink)
}

func detectMinImages(p templates.GuideParams, pages []parsedPage) bool {
	count := 0
	for _, pg := range pages {
		lint.WalkDOM(pg.Doc, func(n *html.Node) {
			if n.Type == html.ElementNode && n.Data == "img" && !isLikelyIcon(n) {
				count++
			}
		})
	}
	return count >= p.Min
}

func detectMinLinks(p templates.GuideParams, pages []parsedPage) bool {
	count := 0
	for _, pg := range pages {
		lint.WalkDOM(pg.Doc, func(n *html.Node) {
			if n.Type != html.ElementNode || n.Data != "a" {
				return
			}
			for _, a := range n.Attr {
				if a.Key == "href" && lint.IsExternalLink(strings.TrimSpace(a.Val)) {
					count++
				}
			}
		})
	}
	return count >= p.Min
}

// --- shared helpers ---------------------------------------------------------

// anyPage reports whether fn matches on at least one page.
func anyPage(pages []parsedPage, fn func(*html.Node) bool) bool {
	for _, pg := range pages {
		if fn(pg.Doc) {
			return true
		}
	}
	return false
}

// hasLinkPrefix reports whether the document has an <a> whose href (trimmed,
// lowercased) begins with prefix — e.g. "tel:" or "mailto:".
func hasLinkPrefix(doc *html.Node, prefix string) bool {
	found := false
	lint.WalkDOM(doc, func(n *html.Node) {
		if found || n.Type != html.ElementNode || n.Data != "a" {
			return
		}
		for _, a := range n.Attr {
			if a.Key == "href" && strings.HasPrefix(strings.ToLower(strings.TrimSpace(a.Val)), prefix) {
				found = true
			}
		}
	})
	return found
}

// hasElement reports whether any element with the given tag exists.
func hasElement(doc *html.Node, tag string) bool {
	found := false
	lint.WalkDOM(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			found = true
		}
	})
	return found
}

// hasNonEmptyElement reports whether an element with the given tag exists and
// carries non-whitespace text.
func hasNonEmptyElement(doc *html.Node, tag string) bool {
	found := false
	lint.WalkDOM(doc, func(n *html.Node) {
		if found || n.Type != html.ElementNode || n.Data != tag {
			return
		}
		if strings.TrimSpace(collectText(n)) != "" {
			found = true
		}
	})
	return found
}

// hasMapLink reports whether the page links or embeds a known maps provider.
func hasMapLink(doc *html.Node) bool {
	found := false
	lint.WalkDOM(doc, func(n *html.Node) {
		if found || n.Type != html.ElementNode {
			return
		}
		var key string
		switch n.Data {
		case "a":
			key = "href"
		case "iframe":
			key = "src"
		default:
			return
		}
		for _, a := range n.Attr {
			if a.Key == key && isMapHost(a.Val) {
				found = true
			}
		}
	})
	return found
}

// isMapHost reports whether a URL points at a recognised maps provider. Scoped
// by host (and path for the generic google.com/maps case) to avoid treating any
// google.com link as a map.
func isMapHost(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Host)
	pathl := strings.ToLower(u.Path)
	switch {
	case strings.Contains(host, "maps.google."):
		return true
	case strings.Contains(host, "google.") && strings.HasPrefix(pathl, "/maps"):
		return true
	case strings.Contains(host, "maps.apple.com"):
		return true
	case strings.Contains(host, "openstreetmap.org"):
		return true
	case host == "goo.gl" && strings.HasPrefix(pathl, "/maps"):
		return true
	case strings.Contains(host, "g.page"):
		return true
	}
	return false
}

// isLikelyIcon filters out logos/avatars/sub-48px images so min_images counts
// real content imagery, not chrome.
func isLikelyIcon(n *html.Node) bool {
	var src, alt string
	for _, a := range n.Attr {
		switch a.Key {
		case "width", "height":
			v, err := strconv.Atoi(strings.TrimSpace(a.Val))
			if err == nil && v < 48 {
				return true
			}
		case "src":
			src = strings.ToLower(a.Val)
		case "alt":
			alt = strings.ToLower(a.Val)
		}
	}
	for _, kw := range []string{"icon", "logo", "avatar"} {
		if strings.Contains(src, kw) || strings.Contains(alt, kw) {
			return true
		}
	}
	return false
}

// block is one heading or run of body text in document order — the linearised
// form headingMatch and sectionPresent reason over.
type block struct {
	heading bool
	level   int    // 1..6 for headings
	text    string // raw concatenated text
}

// flattenBlocks linearises a document into headings and body-text runs in
// document order. Headings are captured whole (not descended into); script,
// style, head and other non-visible subtrees are skipped so their text never
// counts as section body.
func flattenBlocks(doc *html.Node) []block {
	var blocks []block
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "head", "noscript", "template":
				return
			}
			if isHeading(n.Data) {
				blocks = append(blocks, block{heading: true, level: headingLevel(n.Data), text: collectText(n)})
				return
			}
		}
		if n.Type == html.TextNode {
			if strings.TrimSpace(n.Data) != "" {
				blocks = append(blocks, block{text: n.Data})
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return blocks
}

// headingMatch reports whether any heading's normalized text contains a keyword.
func headingMatch(doc *html.Node, keywords []string) bool {
	for _, b := range flattenBlocks(doc) {
		if b.heading && containsAny(normalizeText(b.text), keywords) {
			return true
		}
	}
	return false
}

// sectionPresent reports whether a heading matches a keyword AND the section
// under it (up to the next same-or-higher-level heading) has enough body text
// to be more than an empty placeholder.
func sectionPresent(doc *html.Node, keywords []string) bool {
	blocks := flattenBlocks(doc)
	for i, b := range blocks {
		if !b.heading || !containsAny(normalizeText(b.text), keywords) {
			continue
		}
		count := 0
		for j := i + 1; j < len(blocks); j++ {
			nb := blocks[j]
			if nb.heading && nb.level <= b.level {
				break // next section of equal/higher rank
			}
			count += nonWhitespaceLen(nb.text)
		}
		if count >= sectionMinBodyChars {
			return true
		}
	}
	return false
}

func isHeading(tag string) bool {
	return len(tag) == 2 && tag[0] == 'h' && tag[1] >= '1' && tag[1] <= '6'
}

func headingLevel(tag string) int { return int(tag[1] - '0') }

// collectText concatenates all descendant text of a node.
func collectText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(m *html.Node) {
		if m.Type == html.TextNode {
			b.WriteString(m.Data)
		}
		for c := m.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// normalizeText lowercases and collapses runs of whitespace to single spaces.
func normalizeText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// containsAny reports whether haystack contains any keyword (case-insensitive
// substring; haystack is assumed already lowercased by the caller).
func containsAny(haystack string, keywords []string) bool {
	for _, kw := range keywords {
		if strings.Contains(haystack, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// nonWhitespaceLen counts non-whitespace runes in s. unicode.IsSpace also
// treats the non-breaking space (common in agent-emitted HTML) as whitespace,
// so a section padded only with &nbsp; doesn't read as real content.
func nonWhitespaceLen(s string) int {
	count := 0
	for _, r := range s {
		if !unicode.IsSpace(r) {
			count++
		}
	}
	return count
}
