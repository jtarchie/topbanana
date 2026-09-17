package lint

import (
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// All four report as warnings: each rests on a convention an author may reasonably break, and a false positive would cost a build over a judgement call.

// checkPageStructure runs the heading, landmark, tab-order, and nesting rules over one page.
func checkPageStructure(pi pageInfo) []Error {
	errs := checkHeadingOrder(pi)
	errs = append(errs, checkMainLandmark(pi)...)
	errs = append(errs, checkTabIndex(pi)...)
	errs = append(errs, checkNestedInteractive(pi)...)
	return errs
}

// Only skipped levels are flagged; a page that starts at <h2> is a style choice, a jump from h2 to h4 is not.
func checkHeadingOrder(pi pageInfo) []Error {
	var errs []Error
	prev := 0
	for _, n := range pi.elements {
		level := headingLevel(n)
		if level == 0 || hiddenFromAT(n) {
			continue
		}
		if prev != 0 && level > prev+1 {
			errs = append(errs, Error{
				File: pi.name, Kind: KindHeadingOrder,
				Message: fmt.Sprintf(
					`skipped heading level — <h%d>%s follows an <h%d>, so the outline jumps a level. Screen-reader users navigate by heading level and read a jump as a missing section. Use <h%d> here, or promote the heading above it.`,
					level, quoteText(visibleText(n)), prev, prev+1),
			})
		}
		prev = level
	}
	return errs
}

func headingLevel(n *html.Node) int {
	if len(n.Data) == 2 && n.Data[0] == 'h' && n.Data[1] >= '1' && n.Data[1] <= '6' {
		return int(n.Data[1] - '0')
	}
	// An explicit role=heading carries its level in aria-level, defaulting to 2.
	if role, ok := elementRole(n); ok && role == "heading" && hasAttr(n, "role") {
		lvl, err := strconv.Atoi(strings.TrimSpace(attrVal(n, "aria-level")))
		if err == nil {
			return lvl
		}
		return 2
	}
	return 0
}

// <main> is what a "skip to content" link and every screen reader's landmark menu target.
func checkMainLandmark(pi pageInfo) []Error {
	count := 0
	for _, n := range pi.elements {
		if hiddenFromAT(n) {
			continue
		}
		if role, ok := elementRole(n); ok && role == "main" {
			count++
		}
	}
	switch {
	case count == 0:
		return []Error{{
			File: pi.name, Kind: KindLandmarkMain,
			Message: `no main landmark — this page has no <main> element, so screen-reader and keyboard users have no way to jump past the header and navigation to the actual content. Wrap the page's primary content in <main>…</main>.`,
		}}
	case count > 1:
		return []Error{{
			File: pi.name, Kind: KindLandmarkMain,
			Message: fmt.Sprintf(
				`duplicate main landmark — this page has %d <main> landmarks. Only one can be the primary content, so "skip to content" and the landmark menu become ambiguous. Keep one <main> and make the others <section> or <div>.`,
				count),
		}}
	}
	return nil
}

// A positive tabindex reorders the whole page's tab sequence, not just this element's place in it.
func checkTabIndex(pi pageInfo) []Error {
	var errs []Error
	for _, n := range pi.elements {
		raw, ok := attrLookup(n, "tabindex")
		if !ok {
			continue
		}
		v, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || v < 1 {
			continue
		}
		errs = append(errs, Error{
			File: pi.name, Kind: KindPositiveTabindex,
			Message: fmt.Sprintf(
				`positive tabindex — <%s tabindex=%q> pulls this element to the front of the whole page's tab order, ahead of everything with the default order, and every other positive value has to be kept in sync with it forever. Use tabindex="0" to make it focusable in document order, or reorder the markup instead.`,
				n.Data, raw),
		})
	}
	return errs
}

// Native interactive elements; the browser makes each one a single tab stop.
var interactiveElements = map[string]bool{
	"a":        true,
	"button":   true,
	"input":    true,
	"select":   true,
	"textarea": true,
	"summary":  true,
}

var interactiveRoles = map[string]bool{
	"button":     true,
	"checkbox":   true,
	"combobox":   true,
	"link":       true,
	"menuitem":   true,
	"radio":      true,
	"slider":     true,
	"spinbutton": true,
	"switch":     true,
	"tab":        true,
	"textbox":    true,
}

// Two controls sharing one tab stop collapse into a single confusing announcement, and the inner one is unreachable.
func checkNestedInteractive(pi pageInfo) []Error {
	var errs []Error
	for _, n := range pi.elements {
		if !isInteractive(n) || hiddenFromAT(n) {
			continue
		}
		outer := nearestInteractiveAncestor(n)
		if outer == nil {
			continue
		}
		errs = append(errs, Error{
			File: pi.name, Kind: KindNestedInteractive,
			Message: fmt.Sprintf(
				`nested interactive controls — a <%s> sits inside a <%s>. Both want to be their own tab stop and their own click target, so browsers and screen readers disagree about which one the visitor activated, and the inner control is often unreachable by keyboard. Put them side by side instead of nesting them.`,
				n.Data, outer.Data),
		})
	}
	return errs
}

func isInteractive(n *html.Node) bool {
	if n.Data == "input" && strings.EqualFold(attrVal(n, "type"), "hidden") {
		return false
	}
	// <a> without href is not focusable, and <option> is part of its select rather than a stop of its own.
	if n.Data == "a" && !hasAttr(n, "href") {
		return false
	}
	if interactiveElements[n.Data] {
		return true
	}
	role, ok := elementRole(n)
	return ok && hasAttr(n, "role") && interactiveRoles[role]
}

func nearestInteractiveAncestor(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		// A select owns its options and a label wraps its control on purpose; neither is a nesting bug.
		if p.Data == "select" || p.Data == "datalist" || p.Data == "label" {
			return nil
		}
		if isInteractive(p) {
			return p
		}
	}
	return nil
}

func quoteText(s string) string {
	if s == "" {
		return ""
	}
	const maxQuoted = 40
	if r := []rune(s); len(r) > maxQuoted {
		s = string(r[:maxQuoted]) + "…"
	}
	return fmt.Sprintf(" (%q)", s)
}
