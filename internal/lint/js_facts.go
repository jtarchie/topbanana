package lint

import (
	"regexp"
	"strings"

	"github.com/dop251/goja/ast"
)

// jsRef is one harvested literal plus the ordinal of the inline <script> it
// came from, so error messages can point at "inline <script> #2".
type jsRef struct {
	ordinal int
	value   string
}

// jsFacts is everything the JS-aware checks need from a page's inline
// scripts, harvested in one AST pass per script via the jsWalker hooks.
type jsFacts struct {
	fetchTargets   []jsRef         // fetch('...') first-arg string literals
	domQueries     []jsRef         // getElementById('x') / querySelector('#x') → "x"
	declaredNames  map[string]bool // top-level fns, var/let/const, window.X =
	stringLiterals []string        // every literal — the unreferenced-page scan
	dynamicDOM     bool            // page builds DOM at runtime (gate for id checks)
	parseFailed    bool            // some lintable script didn't parse
}

// dynamicDOMMarkers are substrings whose presence in any inline script means
// the page creates or mutates elements at runtime, so a static id inventory
// is incomplete and id-based JS checks must stand down. setAttribute is
// included as cheap insurance against setAttribute('id', ...). A substring
// scan (not AST) on purpose: it must also fire for scripts that fail to
// parse, and a false "dynamic" only suppresses errors.
var dynamicDOMMarkers = []string{
	"createElement",
	"innerHTML",
	"insertAdjacentHTML",
	"outerHTML",
	"document.write",
	"setAttribute",
}

// idSelectorRE matches a querySelector argument that is a pure id selector.
// Compound selectors (classes, attributes, combinators, pseudo-classes) are
// skipped — validating them would mean implementing CSS matching.
var idSelectorRE = regexp.MustCompile(`^#[A-Za-z_][A-Za-z0-9_-]*$`)

// collectJSFacts harvests jsFacts from a page's pre-parsed inline scripts.
func collectJSFacts(filename string, scripts []scriptInfo) jsFacts {
	facts := jsFacts{declaredNames: map[string]bool{}}
	for _, s := range scripts {
		for _, marker := range dynamicDOMMarkers {
			if strings.Contains(s.text, marker) {
				facts.dynamicDOM = true
				break
			}
		}
		if s.program == nil {
			facts.parseFailed = true
			continue
		}
		ordinal := s.ordinal
		var discard []Error
		w := &jsWalker{
			filename:    filename,
			errs:        &discard,
			collectOnly: true,
			onString: func(lit string) {
				facts.stringLiterals = append(facts.stringLiterals, lit)
			},
			onCall: func(c *ast.CallExpression) {
				recordCall(&facts, ordinal, c)
			},
			onTopName: func(name string) {
				facts.declaredNames[name] = true
			},
		}
		w.walkProgram(s.program)
	}
	return facts
}

// recordCall harvests the two call shapes the checks care about: fetch with a
// string-literal first argument, and document.getElementById /
// anything.querySelector with a literal id(-selector) argument. querySelector
// is accepted on any receiver because el.querySelector('#x') still requires
// the id to exist somewhere on the page.
func recordCall(facts *jsFacts, ordinal int, c *ast.CallExpression) {
	lit, ok := firstStringArg(c)
	if !ok {
		return
	}
	switch callee := c.Callee.(type) {
	case *ast.Identifier:
		if string(callee.Name) == "fetch" {
			facts.fetchTargets = append(facts.fetchTargets, jsRef{ordinal: ordinal, value: lit})
		}
	case *ast.DotExpression:
		switch string(callee.Identifier.Name) {
		case "getElementById":
			if root, isIdent := callee.Left.(*ast.Identifier); isIdent && string(root.Name) == "document" {
				facts.domQueries = append(facts.domQueries, jsRef{ordinal: ordinal, value: lit})
			}
		case "querySelector":
			if idSelectorRE.MatchString(lit) {
				facts.domQueries = append(facts.domQueries, jsRef{ordinal: ordinal, value: strings.TrimPrefix(lit, "#")})
			}
		}
	}
}

// firstStringArg returns a call's first argument when it is a plain string
// literal or an expressionless template literal. Dynamic arguments
// (variables, templates with expressions, concatenations) return false —
// only literals are checkable.
func firstStringArg(c *ast.CallExpression) (string, bool) {
	if len(c.ArgumentList) == 0 {
		return "", false
	}
	switch arg := c.ArgumentList[0].(type) {
	case *ast.StringLiteral:
		return string(arg.Value), true
	case *ast.TemplateLiteral:
		if len(arg.Expressions) > 0 || len(arg.Elements) == 0 {
			return "", false
		}
		var b strings.Builder
		for _, el := range arg.Elements {
			b.WriteString(string(el.Parsed))
		}
		return b.String(), true
	}
	return "", false
}
