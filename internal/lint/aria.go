package lint

import (
	"fmt"
	"sort"
	"strings"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/lint/ariadata"
)

// Every judgement here reads the vendored spec table in internal/lint/ariadata, so a wrong answer is a bad implicit-role guess or a stale vendor — never an invented rule.

func ariaConcrete(name string) bool { return ariadata.Concrete(name) }

// facts gates the reference checks like checkDOMQueries: a page that builds markup at runtime may legitimately name ids the source lacks.
func checkARIA(pi pageInfo, facts jsFacts) []Error {
	errs := make([]Error, 0, len(pi.elements))
	for _, n := range pi.elements {
		errs = append(errs, checkRoleValue(pi, n)...)
		errs = append(errs, checkARIAAttrs(pi, n, facts)...)
	}
	errs = append(errs, checkRoleRelationships(pi)...)
	return errs
}

// checkRoleValue rejects role= values that no element may carry.
func checkRoleValue(pi pageInfo, n *html.Node) []Error {
	raw, ok := attrLookup(n, "role")
	if !ok {
		return nil
	}
	tokens := strings.Fields(raw)
	if len(tokens) == 0 {
		return []Error{{
			File: pi.name, Kind: KindARIAInvalidRole,
			Message: fmt.Sprintf(
				`invalid ARIA role — <%s> has an empty role="" attribute, which leaves the element with no role at all rather than the one that was intended. Remove the attribute or give it a real role.`,
				n.Data),
		}}
	}
	// A browser takes the first role it knows, so the element is only broken when no token is usable.
	for _, tok := range tokens {
		if ariaConcrete(tok) {
			return nil
		}
	}

	bad := tokens[0]
	var hint string
	switch near := ariadata.NearestRole(bad); {
	case near != "":
		hint = fmt.Sprintf("Did you mean role=%q?", near)
	default:
		r, defined := ariadata.Get(bad)
		if defined && r.Abstract {
			hint = fmt.Sprintf("%q is an abstract role: it exists only for other roles to inherit from and can never be written on an element.", bad)
		} else {
			hint = "Check the spelling against the WAI-ARIA role list, or drop the attribute and let the element's own tag supply its role."
		}
	}
	return []Error{{
		File: pi.name, Kind: KindARIAInvalidRole,
		Message: fmt.Sprintf(
			`invalid ARIA role — <%s role=%q> is not a role assistive technology recognizes, so the element is announced as whatever its tag says instead. %s`,
			n.Data, raw, hint),
	}}
}

// Order matters: a misspelled name or a malformed value makes the later role questions meaningless.
func checkARIAAttrs(pi pageInfo, n *html.Node, facts jsFacts) []Error {
	var errs []Error
	role, roleKnown := elementRole(n)

	for _, a := range n.Attr {
		name := strings.ToLower(a.Key)
		if !strings.HasPrefix(name, "aria-") {
			continue
		}

		if !ariadata.IsAttr(name) {
			errs = append(errs, Error{
				File: pi.name, Kind: KindARIAInvalidAttr,
				Message: fmt.Sprintf(
					`invalid ARIA attribute — <%s %s=%q> uses an attribute the ARIA spec does not define, so browsers and screen readers ignore it entirely. %s`,
					n.Data, name, a.Val, suggestAttr(name)),
			})
			continue
		}

		if ok, allowed := ariadata.CheckValue(name, a.Val); !ok {
			errs = append(errs, Error{
				File: pi.name, Kind: KindARIAInvalidAttrValue,
				Message: fmt.Sprintf(
					`invalid ARIA value — <%s %s=%q> is outside what %s accepts, so assistive technology treats the state as unset rather than guessing. Use %s.`,
					n.Data, name, a.Val, name, strings.Join(allowed, ", ")),
			})
			continue
		}

		errs = append(errs, checkARIARefs(pi, n, name, a.Val, facts)...)
		errs = append(errs, checkAttrAllowed(pi, n, name, role, roleKnown)...)
	}
	return errs
}

// checkARIARefs resolves the ids an attribute names against the page.
func checkARIARefs(pi pageInfo, n *html.Node, name, value string, facts jsFacts) []Error {
	_, isRef := ariadata.IsIDRef(name)
	if !isRef || facts.dynamicDOM || facts.parseFailed {
		return nil
	}
	var missing []string
	for _, id := range strings.Fields(value) {
		if pi.ids[id] == 0 {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Error{{
		File: pi.name, Kind: KindARIABrokenRef,
		Message: fmt.Sprintf(
			`broken ARIA reference — <%s %s=%q> names %s, but no element on this page has that id, so the association is silently dropped and the element is announced without it. %s Add the id to the intended element, or point %s at an existing id.`,
			n.Data, name, value, capList(missing, maxListedItems), idInventory(pi), name),
	}}
}

// aria-label on a generic container is dropped rather than honored, which is why prohibition earns its own message.
func checkAttrAllowed(pi pageInfo, n *html.Node, name, role string, roleKnown bool) []Error {
	if !roleKnown {
		return nil
	}
	def, ok := ariadata.Get(role)
	if !ok {
		return nil
	}

	if def.Prohibited[name] {
		return []Error{{
			File: pi.name, Kind: KindARIAProhibitedAttr,
			Message: fmt.Sprintf(
				`ARIA name ignored — <%s %s=...> resolves to role %q, which cannot carry a name: screen readers drop %s here instead of announcing it. Put the text in the element's content, or give the element a role that takes a name (a heading, a region, a button).`,
				n.Data, name, role, name),
		}}
	}
	if !def.Props[name] {
		return []Error{{
			File: pi.name, Kind: KindARIAAllowedAttr,
			Message: fmt.Sprintf(
				`unsupported ARIA state — <%s %s=...> resolves to role %q, which has no %s state, so it is ignored. Move it to the element that actually owns that state, or give this element a role that supports it.`,
				n.Data, name, role, name),
		}}
	}
	return nil
}

// The three rules that need more than the element itself.
func checkRoleRelationships(pi pageInfo) []Error {
	var errs []Error
	for _, n := range pi.elements {
		role, ok := elementRole(n)
		if !ok {
			continue
		}
		def, defined := ariadata.Get(role)
		if !defined {
			continue
		}
		errs = append(errs, checkRequiredAttrs(pi, n, role, def)...)
		errs = append(errs, checkRequiredParent(pi, n, role, def)...)
		errs = append(errs, checkRequiredChildren(pi, n, role, def)...)
	}
	return errs
}

// Explicit role= only: a native <input type="checkbox"> reports checked through the DOM, so demanding aria-checked on it would be wrong.
func checkRequiredAttrs(pi pageInfo, n *html.Node, role string, def ariadata.Role) []Error {
	if len(def.Required) == 0 || !hasAttr(n, "role") {
		return nil
	}
	var missing []string
	for _, attr := range def.Required {
		if !hasAttr(n, attr) {
			missing = append(missing, attr)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []Error{{
		File: pi.name, Kind: KindARIARequiredAttr,
		Message: fmt.Sprintf(
			`incomplete ARIA role — <%s role=%q> is missing %s. That state has no default, so assistive technology announces the role without ever saying what it is set to. Add %s, and keep it in sync whenever the control changes.`,
			n.Data, role, capList(missing, maxListedItems), strings.Join(missing, " and ")),
	}}
}

// checkRequiredParent flags a role placed outside the container that gives it meaning.
func checkRequiredParent(pi pageInfo, n *html.Node, role string, def ariadata.Role) []Error {
	if len(def.RequiredContext) == 0 || !hasAttr(n, "role") {
		return nil
	}
	want := map[string]bool{}
	for _, r := range def.RequiredContext {
		want[r] = true
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		pr, ok := elementRole(p)
		if !ok {
			continue
		}
		if want[pr] {
			return nil
		}
		// A presentational wrapper is transparent; anything else with a real role ends the search.
		if pr != "generic" && pr != "presentation" && pr != "none" {
			break
		}
	}
	sorted := append([]string(nil), def.RequiredContext...)
	sort.Strings(sorted)
	return []Error{{
		File: pi.name, Kind: KindARIARequiredParent,
		Message: fmt.Sprintf(
			`orphaned ARIA role — <%s role=%q> has to sit inside %s to mean anything, and nothing above it does. Wrap it in the missing container, or drop the role.`,
			n.Data, role, joinOr(sorted)),
	}}
}

// aria-owns exempts a widget whose children are adopted from elsewhere in the document.
func checkRequiredChildren(pi pageInfo, n *html.Node, role string, def ariadata.Role) []Error {
	if len(def.RequiredOwned) == 0 || !hasAttr(n, "role") || hasAttr(n, "aria-owns") {
		return nil
	}
	if n.FirstChild == nil {
		return nil
	}
	present := map[string]bool{}
	collectDescendantRoles(n, n, present)
	for _, alternative := range def.RequiredOwned {
		matched := true
		for _, want := range alternative {
			if !present[want] {
				matched = false
				break
			}
		}
		if matched {
			return nil
		}
	}
	options := make([]string, 0, len(def.RequiredOwned))
	for _, alternative := range def.RequiredOwned {
		options = append(options, strings.Join(alternative, " + "))
	}
	sort.Strings(options)
	return []Error{{
		File: pi.name, Kind: KindARIARequiredChildren,
		Message: fmt.Sprintf(
			`incomplete ARIA structure — <%s role=%q> contains none of the child roles it is defined by (%s), so assistive technology announces an empty widget. Give its children the missing role, or drop role=%q and use plain markup.`,
			n.Data, role, joinOr(options), role),
	}}
}

// Stops at any element with a real role of its own, so a nested list's items do not count as the outer list's.
func collectDescendantRoles(root, n *html.Node, out map[string]bool) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type != html.ElementNode {
			continue
		}
		role, ok := elementRole(c)
		if ok {
			out[role] = true
		}
		if ok && role != "generic" && role != "presentation" && role != "none" {
			continue
		}
		collectDescendantRoles(root, c, out)
	}
}

func suggestAttr(name string) string {
	if near := ariadata.NearestAttr(name); near != "" {
		return fmt.Sprintf("Did you mean %s?", near)
	}
	return "Check it against the ARIA states and properties list — misspelled aria-* attributes fail silently."
}

func joinOr(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	case 2:
		return items[0] + " or " + items[1]
	}
	return strings.Join(items[:len(items)-1], ", ") + ", or " + items[len(items)-1]
}
