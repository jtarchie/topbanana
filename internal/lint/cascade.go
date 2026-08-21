package lint

import (
	"fmt"
	"path"
	"strings"

	"golang.org/x/net/html"
)

// This file catches one specific, repeatedly-costly mistake: resizing an image
// by editing its `width`/`height` attributes while a rule in the site's own
// stylesheet pins that same property to a fixed length.
//
// Those attributes are presentation *hints*, the lowest tier of the cascade —
// any author rule outranks them. So the markup changes, the page does not, and
// nothing in the build reports a problem. It happened twice in a row on one
// site: 84px to 200px, then 200px to 360px, both runs reported completed, and
// the images never moved off the 84px the stylesheet had pinned them to.
//
// Everything here is shaped by one constraint: a lint error gates a build, and
// the agent's only way to clear a false one is to damage the page. So each
// restriction below exists to keep a *correct* site silent, and every one of
// them was a live false positive at some point:
//
//   - <img> only. On canvas the attributes are the bitmap resolution, on
//     inline <svg> they pair with viewBox, and on iframe/video/embed they are
//     the intrinsic box. Overriding those from CSS is correct, and flagging the
//     inline-SVG sprite pattern would contradict what both substrate prompts
//     tell the agent to emit.
//   - Unconditional rules only. A size inside @media applies at that
//     breakpoint; the attribute governs everywhere else, so a mobile override
//     is not a conflict. Skipping at-rules also keeps @keyframes percentages
//     and @font-face descriptors from being read as selectors.
//   - Fixed lengths only. `height:auto`, `width:100%`, `min()`/`clamp()` make
//     an element fluid, which is exactly what sizing attributes are meant to be
//     paired with — they supply the aspect ratio that prevents layout shift.
//   - Ancestors matched for real against the DOM, so `.sidebar .logo` says
//     nothing about a `.logo` outside the sidebar.
//   - Only stylesheets the page actually links.

// sizingRule is one unconditional author-stylesheet declaration that pins width
// or height to a fixed length.
type sizingRule struct {
	file     string // the stylesheet it came from
	selector string // the full selector text, for the error message
	prop     string // "width" or "height"
	value    string // the fixed length, e.g. "84px"
	chain    []compoundSelector
}

// compoundSelector is one link in a selector chain: a tag and/or classes, plus
// whether it is joined to what follows by a child combinator.
type compoundSelector struct {
	tag     string
	classes []string
	child   bool
}

// sizedProps are the two properties that override the same-named HTML
// attribute outright, in fixed order so a page with both conflicts always
// produces the same errors in the same sequence — iterating the declaration
// map directly made the reported property vary between identical runs.
// max-/min- variants are excluded: they constrain rather than pin, and pairing
// them with sizing attributes is normal practice.
var sizedProps = []string{"width", "height"}

// collectSizingRules parses an author stylesheet for unconditional rules that
// pin width or height to a fixed length. The parse is a brace scanner rather
// than a real CSS parser, and it drops anything it doesn't fully understand: a
// missed rule costs a missed warning, while a misparse costs a build.
func collectSizingRules(file, css string) []sizingRule {
	var rules []sizingRule
	for _, block := range topLevelRuleBlocks(stripCSSComments(css)) {
		decls := parseDeclarations(block.body)
		if len(decls) == 0 {
			continue
		}
		for _, selector := range strings.Split(block.prelude, ",") {
			chain, ok := parseSelectorChain(strings.TrimSpace(selector))
			if !ok {
				continue
			}
			for _, prop := range sizedProps {
				value, declared := decls[prop]
				if !declared || !isFixedLength(value) {
					continue
				}
				rules = append(rules, sizingRule{
					file:     file,
					selector: strings.TrimSpace(selector),
					prop:     prop,
					value:    value,
					chain:    chain,
				})
			}
		}
	}
	return rules
}

type ruleBlock struct {
	prelude string
	body    string
}

// topLevelRuleBlocks returns every unconditional (selector list, declarations)
// pair in the sheet. At-rule blocks are skipped whole rather than descended
// into — see the file comment on why a conditional size is not a conflict.
func topLevelRuleBlocks(css string) []ruleBlock {
	var out []ruleBlock
	for len(css) > 0 {
		open := strings.IndexByte(css, '{')
		if open < 0 {
			break
		}
		prelude := css[:open]
		// A statement at-rule (`@charset "utf-8";`, `@import url(…);`,
		// `@layer a, b;`) ends at its semicolon. Without this the prelude runs
		// from the previous `}` and swallows the following selector, silently
		// dropping the first rule of any sheet that opens with one.
		if semi := strings.LastIndexByte(prelude, ';'); semi >= 0 {
			prelude = prelude[semi+1:]
		}
		prelude = strings.TrimSpace(prelude)

		body, rest, ok := balancedBlock(css[open:])
		if !ok {
			break
		}
		css = rest
		if strings.HasPrefix(prelude, "@") {
			continue
		}
		out = append(out, ruleBlock{prelude: prelude, body: body})
	}
	return out
}

// balancedBlock consumes a `{...}` starting at s[0] and returns its inner text
// plus whatever follows. ok is false on an unbalanced sheet, which ends the
// scan rather than guessing.
func balancedBlock(s string) (body, rest string, ok bool) {
	depth := 0
	for i := range len(s) {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return s[1:i], s[i+1:], true
			}
		}
	}
	return "", "", false
}

// parseDeclarations splits a declaration body into property→value, with any
// `!important` flag stripped from the value. Dropping the flag matters: it is
// the strongest override in the cascade, so a rule carrying it is the most
// likely cause of the symptom this check exists to explain — and left attached
// it made the value fail the length test and disappear entirely.
func parseDeclarations(body string) map[string]string {
	out := map[string]string{}
	for _, decl := range strings.Split(body, ";") {
		prop, value, found := strings.Cut(decl, ":")
		if !found || strings.ContainsAny(decl, "{}") {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		value = strings.TrimSpace(value)
		if idx := strings.LastIndex(strings.ToLower(value), "!important"); idx >= 0 {
			value = strings.TrimSpace(value[:idx])
		}
		if prop == "" || value == "" {
			continue
		}
		out[prop] = value
	}
	return out
}

// parseSelectorChain splits a selector into its compounds, outermost first.
// Reports ok=false for anything this check can't reason about — pseudo-classes,
// attribute selectors, ids, `*`, sibling combinators — so a `:hover` size or an
// `[open]` size is never mistaken for a baseline one.
func parseSelectorChain(selector string) ([]compoundSelector, bool) {
	if selector == "" || strings.ContainsAny(selector, ":[]*&|#+~") {
		return nil, false
	}
	var chain []compoundSelector
	nextIsChild := false
	for _, token := range strings.Fields(strings.ReplaceAll(selector, ">", " > ")) {
		if token == ">" {
			nextIsChild = true
			continue
		}
		parts := strings.Split(token, ".")
		compound := compoundSelector{tag: strings.ToLower(parts[0]), child: nextIsChild}
		for _, class := range parts[1:] {
			if class == "" {
				return nil, false
			}
			compound.classes = append(compound.classes, class)
		}
		if compound.tag == "" && len(compound.classes) == 0 {
			return nil, false
		}
		chain = append(chain, compound)
		nextIsChild = false
	}
	if len(chain) == 0 || nextIsChild {
		return nil, false
	}
	return chain, true
}

// isFixedLength reports whether a CSS value pins a size outright. Percentages,
// auto, and any function (min/max/clamp/calc) are deliberately excluded.
func isFixedLength(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" || strings.ContainsAny(value, "(%") {
		return false
	}
	for _, unit := range []string{"px", "rem", "em", "pt", "cm", "mm", "in", "pc"} {
		if !strings.HasSuffix(value, unit) {
			continue
		}
		number := strings.TrimSuffix(value, unit)
		if number == "" {
			return false
		}
		for _, r := range number {
			if (r < '0' || r > '9') && r != '.' && r != '-' && r != '+' {
				return false
			}
		}
		return true
	}
	return false
}

// checkCascadeConflicts reports images whose width/height attribute is
// overridden by a fixed size in a stylesheet that page links.
func checkCascadeConflicts(pages []pageInfo, rulesByFile map[string][]sizingRule) []Error {
	if len(rulesByFile) == 0 {
		return nil
	}
	var errs []Error
	for _, page := range pages {
		rules := rulesForPage(page, rulesByFile)
		if len(rules) == 0 {
			continue
		}
		for _, el := range page.elements {
			if el.Data != "img" {
				continue
			}
			errs = append(errs, conflictsForElement(page.name, el, rules)...)
		}
	}
	return errs
}

// conflictsForElement reports at most one conflict per sized property, so a
// rule pinning both width and height yields both errors and the agent clears
// them in a single pass instead of one per lint retry.
func conflictsForElement(file string, el *html.Node, rules []sizingRule) []Error {
	var errs []Error
	for _, prop := range sizedProps {
		attr := attrValue(el, prop)
		if attr == "" {
			continue
		}
		for _, rule := range rules {
			if rule.prop != prop || sameLength(attr, rule.value) || !selectorMatches(rule.chain, el) {
				continue
			}
			errs = append(errs, Error{
				File: file,
				Kind: KindCascadeConflict,
				Message: fmt.Sprintf(
					"<img> sets %s=%q, but %s has `%s { %s: %s }` — a stylesheet rule always beats a width/height attribute, so this image renders at %s. Change the rule in %s instead of the attribute.",
					prop, attr, rule.file, rule.selector, rule.prop, rule.value, rule.value, rule.file),
			})
			break
		}
	}
	return errs
}

// rulesForPage returns the rules from stylesheets this page actually links. A
// site can carry a sheet only some pages use; applying its rules everywhere
// would report a conflict with CSS that never loads on that page.
func rulesForPage(page pageInfo, rulesByFile map[string][]sizingRule) []sizingRule {
	var rules []sizingRule
	for _, href := range linkedStylesheets(page) {
		resolved := path.Join(page.dir, href)
		if strings.HasPrefix(href, "/") {
			resolved = path.Clean(strings.TrimPrefix(href, "/"))
		}
		rules = append(rules, rulesByFile[resolved]...)
	}
	return rules
}

// linkedStylesheets returns the href of every same-origin
// <link rel="stylesheet"> on the page.
func linkedStylesheets(page pageInfo) []string {
	var hrefs []string
	for _, el := range page.elements {
		if el.Data != "link" || !strings.Contains(strings.ToLower(attrValue(el, "rel")), "stylesheet") {
			continue
		}
		href := strings.TrimSpace(attrValue(el, "href"))
		if href == "" || IsExternalLink(href) {
			continue
		}
		hrefs = append(hrefs, href)
	}
	return hrefs
}

// selectorMatches walks the chain from its key compound outward, checking each
// ancestor against the real DOM. A descendant link may skip elements; a child
// link may not.
func selectorMatches(chain []compoundSelector, el *html.Node) bool {
	if len(chain) == 0 || !compoundMatches(chain[len(chain)-1], el) {
		return false
	}
	node := el
	for i := len(chain) - 2; i >= 0; i-- {
		// chain[i+1] carries the combinator joining it to chain[i].
		if chain[i+1].child {
			node = elementParent(node)
			if !compoundMatches(chain[i], node) {
				return false
			}
			continue
		}
		for {
			node = elementParent(node)
			if node == nil {
				return false
			}
			if compoundMatches(chain[i], node) {
				break
			}
		}
	}
	return true
}

func compoundMatches(c compoundSelector, n *html.Node) bool {
	if n == nil || n.Type != html.ElementNode {
		return false
	}
	if c.tag != "" && !strings.EqualFold(c.tag, n.Data) {
		return false
	}
	if len(c.classes) == 0 {
		return true
	}
	have := make(map[string]bool)
	for _, class := range strings.Fields(attrValue(n, "class")) {
		have[class] = true
	}
	for _, want := range c.classes {
		if !have[want] {
			return false
		}
	}
	return true
}

func elementParent(n *html.Node) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode {
			return p
		}
	}
	return nil
}

// sameLength reports whether a bare HTML sizing attribute and a CSS length
// describe the same pixel size, so a stylesheet that merely restates the
// attribute isn't reported as fighting it.
func sameLength(attr, cssValue string) bool {
	return strings.TrimSpace(attr)+"px" == strings.ToLower(strings.TrimSpace(cssValue))
}

func attrValue(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if strings.EqualFold(a.Key, name) {
			return a.Val
		}
	}
	return ""
}

// stripCSSComments removes /* … */ spans so a commented-out rule is never read
// as a live one. An unterminated comment swallows the rest of the sheet, which
// matches how a browser parses it.
func stripCSSComments(css string) string {
	var b strings.Builder
	b.Grow(len(css))
	for {
		open := strings.Index(css, "/*")
		if open < 0 {
			b.WriteString(css)
			return b.String()
		}
		b.WriteString(css[:open])
		closed := strings.Index(css[open+2:], "*/")
		if closed < 0 {
			return b.String()
		}
		css = css[open+2+closed+2:]
	}
}
