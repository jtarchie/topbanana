package lint

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
)

// This file owns the dead-interaction checks: things a visitor clicks, taps,
// or a script touches that silently do nothing — labels pointing at no
// control, duplicated ids, contact links that can't be dialed or mailed,
// inline handlers calling functions that don't exist, and DOM lookups for
// ids that were never written. Caveat shared by the id-based checks: ids
// inside <template>/<noscript> are collected like any other (a
// false-negative direction we accept; see collectPageInfo).

// checkDeadInteractions runs all five per-page checks.
func checkDeadInteractions(pi pageInfo, facts jsFacts) []Error {
	errs := checkLabels(pi)
	errs = append(errs, checkDuplicateIDs(pi)...)
	errs = append(errs, checkContactHrefs(pi)...)
	errs = append(errs, checkHandlers(pi, facts)...)
	errs = append(errs, checkDOMQueries(pi, facts)...)
	return errs
}

// idInventory words a page's id list for repair messages.
func idInventory(pi pageInfo) string {
	if len(pi.ids) == 0 {
		return "This page has no element ids at all."
	}
	ids := make([]string, 0, len(pi.ids))
	for id := range pi.ids {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return "Existing ids: " + capList(ids, maxListedItems) + "."
}

// checkLabels flags <label for="x"> with no id="x" on the page — clicking
// the label focuses nothing and screen readers lose the association.
func checkLabels(pi pageInfo) []Error {
	var errs []Error
	seen := map[string]bool{}
	for _, n := range pi.elements {
		if n.Data != "label" {
			continue
		}
		forID := strings.TrimSpace(attrVal(n, "for"))
		if forID == "" || pi.ids[forID] > 0 || seen[forID] {
			continue
		}
		seen[forID] = true
		errs = append(errs, Error{
			File: pi.name,
			Kind: KindOrphanLabel,
			Message: fmt.Sprintf(
				`broken label — <label for=%q> matches no element id on this page, so clicking the label focuses nothing and screen readers can't announce which control it belongs to. %s Give the intended control id=%q, or change for= to the control's real id.`,
				forID, idInventory(pi), forID),
		})
	}
	return errs
}

// checkDuplicateIDs flags ids that appear on more than one element of a page.
func checkDuplicateIDs(pi pageInfo) []Error {
	var dups []string
	for id, count := range pi.ids {
		if count > 1 {
			dups = append(dups, id)
		}
	}
	sort.Strings(dups)
	errs := make([]Error, 0, len(dups))
	for _, id := range dups {
		errs = append(errs, Error{
			File: pi.name,
			Kind: KindDuplicateID,
			Message: fmt.Sprintf(
				`duplicate id — id=%q appears on %d elements of this page. Ids must be unique: anchor links, label/for pairs, and getElementById all resolve to only the first occurrence, so the others silently misbehave. Rename the duplicates so each id appears once.`,
				id, pi.ids[id]),
		})
	}
	return errs
}

// emailRE is a deliberately loose local@domain.tld shape — tight enough to
// catch placeholders and typos (no @, spaces, missing TLD), loose enough to
// never reject a deliverable address.
var emailRE = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// minTelDigits is the floor for a dialable number — short codes exist but
// the agent never writes them; 7 catches "tel:123" placeholders without
// flagging local numbers.
const minTelDigits = 7

// checkContactHrefs validates mailto: and tel: href values — the
// click-to-contact links a non-technical owner never tests and whose
// failure costs them exactly the visitors who tried to reach them.
func checkContactHrefs(pi pageInfo) []Error {
	var errs []Error
	seen := map[string]bool{}
	for _, n := range pi.elements {
		href := strings.TrimSpace(attrVal(n, "href"))
		if href == "" || seen[href] {
			continue
		}
		seen[href] = true
		lower := strings.ToLower(href)
		var e *Error
		switch {
		case strings.HasPrefix(lower, "mailto:"):
			e = checkMailtoHref(pi.name, href)
		case strings.HasPrefix(lower, "tel:"):
			e = checkTelHref(pi.name, href)
		}
		if e != nil {
			errs = append(errs, *e)
		}
	}
	return errs
}

func checkMailtoHref(filename, href string) *Error {
	addr := href[len("mailto:"):]
	if i := strings.IndexByte(addr, '?'); i != -1 {
		addr = addr[:i]
	}
	// PathUnescape, not QueryUnescape: '+' is a legitimate character in an
	// email local part, and QueryUnescape would turn it into a space.
	dec, err := url.PathUnescape(addr)
	if err == nil {
		addr = dec
	}
	ok := addr != ""
	for _, a := range strings.Split(addr, ",") {
		if !emailRE.MatchString(strings.TrimSpace(a)) {
			ok = false
			break
		}
	}
	if ok {
		return nil
	}
	return &Error{
		File: filename,
		Kind: KindBrokenContactHref,
		Message: fmt.Sprintf(
			`broken mailto link — href=%q is not a valid email address (expected user@domain after "mailto:"). The link opens the visitor's mail client addressed to nothing. Use a real address (href="mailto:owner@example.com") or remove the link.`,
			href),
	}
}

func checkTelHref(filename, href string) *Error {
	num := href[len("tel:"):]
	if i := strings.IndexByte(num, '?'); i != -1 {
		num = num[:i]
	}
	digits := 0
	valid := true
	for _, r := range num {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case strings.ContainsRune(" -().+", r):
			// separator — fine
		default:
			valid = false
		}
	}
	if valid && digits >= minTelDigits {
		return nil
	}
	return &Error{
		File: filename,
		Kind: KindBrokenContactHref,
		Message: fmt.Sprintf(
			`broken tel link — href=%q does not contain a dialable phone number (at least %d digits; spaces, dashes, dots, parentheses, and a leading + are fine). Use the full number (href="tel:+15551234567") or remove the link.`,
			href, minTelDigits),
	}
}

// handlerAttrs is the explicit list of inline event-handler attributes the
// undefined-handler check inspects. An explicit set, never a bare "on"
// prefix — that would sweep in lookalike attributes that aren't handlers.
var handlerAttrs = map[string]bool{
	"onclick": true, "ondblclick": true, "onsubmit": true, "onreset": true,
	"onchange": true, "oninput": true, "onselect": true,
	"onkeydown": true, "onkeyup": true, "onkeypress": true,
	"onmouseover": true, "onmouseout": true, "onmouseenter": true, "onmouseleave": true,
	"onfocus": true, "onblur": true, "onload": true,
	"ontouchstart": true, "ontouchend": true, "onscroll": true,
}

// browserGlobals are names an inline handler can call without any page
// script defining them. Generous on purpose — a false "undefined" here
// sends the agent chasing a bug that doesn't exist.
var browserGlobals = map[string]bool{
	"alert": true, "confirm": true, "prompt": true, "print": true,
	"open": true, "close": true, "fetch": true,
	"setTimeout": true, "setInterval": true, "clearTimeout": true, "clearInterval": true,
	"requestAnimationFrame": true, "scroll": true, "scrollTo": true, "scrollBy": true,
	"getComputedStyle": true, "matchMedia": true, "structuredClone": true,
	"encodeURIComponent": true, "decodeURIComponent": true, "encodeURI": true, "decodeURI": true,
	"parseInt": true, "parseFloat": true, "isNaN": true, "isFinite": true,
	"Number": true, "String": true, "Boolean": true, "Date": true, "Array": true,
	"history": true, "location": true, "document": true, "window": true,
	"navigator": true, "localStorage": true, "sessionStorage": true,
	"console": true, "event": true,
}

// checkHandlers flags inline event handlers that call a function no inline
// script on the page defines — the button that throws a ReferenceError and
// does nothing. Only the unambiguous shape is checked: a bare-identifier
// call (`fn()`, `return fn()`, or `fn(); return false`). Member-expression
// handlers and multi-statement handlers are skipped, and the whole check
// stands down when any page script failed to parse (the names may be in
// there).
func checkHandlers(pi pageInfo, facts jsFacts) []Error {
	if facts.parseFailed {
		return nil
	}
	var errs []Error
	seen := map[string]bool{}
	for _, n := range pi.elements {
		for _, a := range n.Attr {
			if !handlerAttrs[a.Key] {
				continue
			}
			name, ok := simpleHandlerCallee(a.Val)
			if !ok || facts.declaredNames[name] || browserGlobals[name] || seen[name] {
				continue
			}
			seen[name] = true
			errs = append(errs, Error{
				File: pi.name,
				Kind: KindUndefinedHandler,
				Message: fmt.Sprintf(
					`undefined handler — %s=%q calls %s() but no inline <script> on this page defines it (top-level functions, var/let/const, and window.* assignments all checked), so the browser throws a ReferenceError and nothing happens. %s Define function %s() in an inline script on this page, or call a function that exists.`,
					a.Key, a.Val, name, definedInventory(facts), name),
			})
		}
	}
	return errs
}

func definedInventory(facts jsFacts) string {
	if len(facts.declaredNames) == 0 {
		return "This page's scripts define no callable names."
	}
	names := make([]string, 0, len(facts.declaredNames))
	for n := range facts.declaredNames {
		names = append(names, n)
	}
	sort.Strings(names)
	return "Defined names on this page: " + capList(names, maxListedItems) + "."
}

// simpleHandlerCallee extracts the called function name from an inline
// handler value when — and only when — the handler is one unambiguous call
// shape: `fn(...)`, `return fn(...)`, or `fn(...); return true/false`.
// Anything else (member calls, multiple statements, assignments) returns
// false and the handler is not checked.
func simpleHandlerCallee(val string) (string, bool) {
	prog, err := parser.ParseFile(nil, "handler.js", "function __h(){"+val+"\n}", 0)
	if err != nil || len(prog.Body) != 1 {
		return "", false
	}
	fd, ok := prog.Body[0].(*ast.FunctionDeclaration)
	if !ok || fd.Function == nil || fd.Function.Body == nil {
		return "", false
	}
	stmts := make([]ast.Statement, 0, len(fd.Function.Body.List))
	for _, s := range fd.Function.Body.List {
		if _, isEmpty := s.(*ast.EmptyStatement); isEmpty {
			continue
		}
		stmts = append(stmts, s)
	}

	call := handlerCall(stmts)
	if call == nil {
		return "", false
	}
	id, ok := call.Callee.(*ast.Identifier)
	if !ok {
		return "", false
	}
	return string(id.Name), true
}

// handlerCall picks the single call expression out of the accepted handler
// statement shapes, nil for anything else.
func handlerCall(stmts []ast.Statement) *ast.CallExpression {
	switch len(stmts) {
	case 1:
		switch s := stmts[0].(type) {
		case *ast.ExpressionStatement:
			call, _ := s.Expression.(*ast.CallExpression)
			return call
		case *ast.ReturnStatement:
			call, _ := s.Argument.(*ast.CallExpression)
			return call
		}
	case 2:
		es, okExpr := stmts[0].(*ast.ExpressionStatement)
		rs, okRet := stmts[1].(*ast.ReturnStatement)
		if okExpr && okRet {
			if _, isBool := rs.Argument.(*ast.BooleanLiteral); isBool {
				call, _ := es.Expression.(*ast.CallExpression)
				return call
			}
		}
	}
	return nil
}

// checkDOMQueries flags getElementById/querySelector('#id') literals with no
// matching id on the page — the script that silently gets null and dies. The
// whole page is skipped when its scripts build DOM at runtime
// (facts.dynamicDOM) or failed to parse: a static id inventory would be
// incomplete, and a false positive here blocks a build.
func checkDOMQueries(pi pageInfo, facts jsFacts) []Error {
	if facts.dynamicDOM || facts.parseFailed {
		return nil
	}
	var errs []Error
	seen := map[string]bool{}
	for _, ref := range facts.domQueries {
		if pi.ids[ref.value] > 0 || seen[ref.value] {
			continue
		}
		seen[ref.value] = true
		errs = append(errs, Error{
			File: pi.name,
			Kind: KindBrokenDOMQuery,
			Message: fmt.Sprintf(
				`broken DOM lookup — inline <script> #%d looks up id %q (getElementById/querySelector) but no element on this page has that id, so the script gets null and throws when it touches the result. %s Add id=%q to the intended element, or query an existing id.`,
				ref.ordinal, ref.value, idInventory(pi), ref.value),
		})
	}
	return errs
}
