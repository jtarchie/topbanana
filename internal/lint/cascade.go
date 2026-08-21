package lint

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// This file catches one specific, repeatedly-costly mistake: resizing an
// element by editing its `width`/`height` attributes while a rule in the
// site's own stylesheet pins that same property to a fixed length.
//
// Those attributes are presentation *hints*, the lowest tier of the cascade —
// any author rule outranks them. So the markup changes, the page does not, and
// nothing in the build reports a problem. It happened twice in a row on one
// site: 84px to 200px, then 200px to 360px, both runs reported completed, and
// the images never moved off the 84px the stylesheet had pinned them to.
//
// The check is deliberately narrow, because lint errors gate a build. It fires
// only when a rule sets a *fixed length* that differs from the attribute. A
// rule making an element fluid — `height:auto`, `width:100%`, `max-width` —
// is the correct responsive pattern paired with sizing attributes that supply
// an aspect ratio, and must never be flagged.

// sizingRule is one author-stylesheet declaration that pins width or height to
// a fixed length, along with just enough of its selector to tell which
// elements it could apply to.
type sizingRule struct {
	file     string // the stylesheet it came from
	selector string // the full selector text, for the error message
	prop     string // "width" or "height"
	value    string // the fixed length, e.g. "84px"
	tag      string // element name in the key compound, empty if none
	classes  []string
}

// fixedSizeProps are the two properties that override the same-named HTML
// attribute outright. max-/min- variants are excluded: they constrain rather
// than pin, and pairing them with sizing attributes is normal practice.
var fixedSizeProps = map[string]bool{"width": true, "height": true}

// collectSizingRules parses an author stylesheet for rules that pin width or
// height to a fixed length. The parse is a brace scanner rather than a real
// CSS parser: it descends into @media and @supports blocks (a size pinned
// inside a breakpoint is exactly as binding as one outside) and ignores
// everything else it doesn't understand, because a missed rule costs a missed
// warning while a misparse would cost a false build failure.
func collectSizingRules(file, css string) []sizingRule {
	var rules []sizingRule
	for _, block := range flattenRuleBlocks(stripCSSComments(css)) {
		decls := parseDeclarations(block.body)
		if len(decls) == 0 {
			continue
		}
		for _, selector := range strings.Split(block.prelude, ",") {
			selector = strings.TrimSpace(selector)
			if selector == "" {
				continue
			}
			tag, classes, ok := keyCompound(selector)
			if !ok {
				continue
			}
			for prop, value := range decls {
				if !fixedSizeProps[prop] || !isFixedLength(value) {
					continue
				}
				rules = append(rules, sizingRule{
					file:     file,
					selector: selector,
					prop:     prop,
					value:    value,
					tag:      tag,
					classes:  classes,
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

// flattenRuleBlocks returns every (selector list, declarations) pair in the
// sheet, descending through conditional at-rules. Blocks whose prelude is an
// at-rule we don't recognise are skipped entirely — @keyframes percentages and
// @font-face descriptors are not selectors and must not be read as any.
func flattenRuleBlocks(css string) []ruleBlock {
	var out []ruleBlock
	for len(css) > 0 {
		open := strings.IndexByte(css, '{')
		if open < 0 {
			break
		}
		prelude := strings.TrimSpace(css[:open])
		body, rest, ok := balancedBlock(css[open:])
		if !ok {
			break
		}
		css = rest

		if strings.HasPrefix(prelude, "@") {
			if conditionalAtRules[atRuleName(prelude)] {
				out = append(out, flattenRuleBlocks(body)...)
			}
			continue
		}
		out = append(out, ruleBlock{prelude: prelude, body: body})
	}
	return out
}

// conditionalAtRules wrap ordinary style rules that still apply. Everything
// else — @keyframes, @font-face, @property — holds blocks that only look like
// style rules, so descending into them would invent selectors.
var conditionalAtRules = map[string]bool{
	"media": true, "supports": true, "layer": true, "container": true, "scope": true,
}

// atRuleName returns the identifier after the @, stopping at whatever ends it.
// The prelude is unnormalised source, so `@media(max-width:520px)` has no
// space to split on and cutting at the first space would yield the whole
// condition as the name.
func atRuleName(prelude string) string {
	name := strings.TrimPrefix(prelude, "@")
	for i, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && r != '-' {
			return strings.ToLower(name[:i])
		}
	}
	return strings.ToLower(name)
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

// parseDeclarations splits a declaration body into property→value. Nested
// blocks are already consumed by the caller, so anything with a brace here is
// malformed and dropped.
func parseDeclarations(body string) map[string]string {
	out := map[string]string{}
	for _, decl := range strings.Split(body, ";") {
		prop, value, found := strings.Cut(decl, ":")
		if !found || strings.ContainsAny(decl, "{}") {
			continue
		}
		prop = strings.ToLower(strings.TrimSpace(prop))
		value = strings.TrimSpace(value)
		if prop == "" || value == "" {
			continue
		}
		out[prop] = value
	}
	return out
}

// keyCompound extracts the tag name and classes from a selector's rightmost
// compound — the part that decides which elements the rule targets. Ancestor
// constraints are dropped, which can only widen the match; that direction is
// safe here because the element must independently carry a conflicting sizing
// attribute before anything is reported.
//
// Returns ok=false for anything with state or structure this check can't
// reason about (pseudo-classes, attribute selectors, `*`), so a `:hover` size
// or an `[open]` size is never mistaken for a baseline one.
func keyCompound(selector string) (tag string, classes []string, ok bool) {
	if strings.ContainsAny(selector, ":[]*&|") {
		return "", nil, false
	}
	compound := selector
	for _, combinator := range []string{">", "+", "~", " "} {
		if idx := strings.LastIndex(compound, combinator); idx >= 0 {
			compound = compound[idx+len(combinator):]
		}
	}
	compound = strings.TrimSpace(compound)
	if compound == "" {
		return "", nil, false
	}

	parts := strings.Split(compound, ".")
	tag = strings.ToLower(strings.TrimSpace(parts[0]))
	if strings.Contains(tag, "#") {
		// An id selector outranks everything anyway; reporting it adds no
		// information the author doesn't already have.
		return "", nil, false
	}
	for _, class := range parts[1:] {
		class = strings.TrimSpace(class)
		if class == "" {
			return "", nil, false
		}
		classes = append(classes, class)
	}
	if tag == "" && len(classes) == 0 {
		return "", nil, false
	}
	return tag, classes, true
}

// isFixedLength reports whether a CSS value pins a size outright. Percentages,
// auto, and any function (min/max/clamp/calc) are deliberately excluded: those
// make an element fluid, which is what sizing attributes are *supposed* to be
// paired with.
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

// checkCascadeConflicts reports elements whose width/height attribute is
// overridden by a fixed size in one of the site's own stylesheets. Rules from
// the generated /app.css are never passed in: it is entirely inside a CSS
// @layer, so it loses to any authored rule and cannot be the cause.
func checkCascadeConflicts(pages []pageInfo, rules []sizingRule) []Error {
	if len(rules) == 0 {
		return nil
	}
	var errs []Error
	for _, page := range pages {
		for _, el := range page.elements {
			for _, rule := range rules {
				attr := attrValue(el, rule.prop)
				if attr == "" || !ruleMatches(rule, el) || sameLength(attr, rule.value) {
					continue
				}
				errs = append(errs, Error{
					File: page.name,
					Kind: KindCascadeConflict,
					Message: fmt.Sprintf(
						"<%s> sets %s=%q, but %s has `%s { %s: %s }` — a stylesheet rule always beats a width/height attribute, so this element renders at %s. Change the rule in %s instead of the attribute.",
						el.Data, rule.prop, attr, rule.file, rule.selector, rule.prop, rule.value, rule.value, rule.file),
				})
				// One report per element and property is enough; further
				// matching rules would say the same thing.
				break
			}
		}
	}
	return errs
}

// ruleMatches reports whether the rule's key compound could select el: every
// class in the compound present, and the tag name agreeing if the rule names
// one.
func ruleMatches(rule sizingRule, el *html.Node) bool {
	if rule.tag != "" && !strings.EqualFold(rule.tag, el.Data) {
		return false
	}
	if len(rule.classes) == 0 {
		return true
	}
	have := make(map[string]bool)
	for _, class := range strings.Fields(attrValue(el, "class")) {
		have[class] = true
	}
	for _, want := range rule.classes {
		if !have[want] {
			return false
		}
	}
	return true
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
// as a live one. Unterminated comments swallow the rest of the sheet, which
// matches how a browser parses them.
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
