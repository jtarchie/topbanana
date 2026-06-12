package lint

import (
	"testing"
)

func factsOf(t *testing.T, js string) jsFacts {
	t.Helper()
	pi := pageOf(t, "index.html", `<!DOCTYPE html><html><body><script>`+js+`</script></body></html>`)
	return collectJSFacts("index.html", pi.scripts)
}

func TestCollectJSFacts_FetchTargets(t *testing.T) {
	t.Parallel()

	facts := factsOf(t, `
fetch('/api/list');
fetch("data.json", {method: 'GET'});
fetch(`+"`/api/orders`"+`);
fetch(url);            // dynamic — skipped
fetch('/api/' + name); // concatenation — skipped
`)

	want := []string{"/api/list", "data.json", "/api/orders"}
	if len(facts.fetchTargets) != len(want) {
		t.Fatalf("fetchTargets = %+v, want %d entries", facts.fetchTargets, len(want))
	}
	for i, w := range want {
		if facts.fetchTargets[i].value != w {
			t.Errorf("fetchTargets[%d] = %q, want %q", i, facts.fetchTargets[i].value, w)
		}
	}
}

func TestCollectJSFacts_DOMQueries(t *testing.T) {
	t.Parallel()

	facts := factsOf(t, `
document.getElementById('wall');
document.querySelector('#count');
el.querySelector('#nested');
document.querySelector('.card');        // class selector — skipped
document.querySelector('#a .b');        // compound — skipped
document.getElementById(dynamic);       // non-literal — skipped
window.getElementById('nope');          // wrong receiver — skipped
`)

	want := []string{"wall", "count", "nested"}
	if len(facts.domQueries) != len(want) {
		t.Fatalf("domQueries = %+v, want %d entries", facts.domQueries, len(want))
	}
	for i, w := range want {
		if facts.domQueries[i].value != w {
			t.Errorf("domQueries[%d] = %q, want %q", i, facts.domQueries[i].value, w)
		}
	}
}

func TestCollectJSFacts_DeclaredNames(t *testing.T) {
	t.Parallel()

	facts := factsOf(t, `
function topFn() {}
var oldVar = 1;
let lexical = 2;
const arrow = () => {};
window.attached = function() {};
function outer() { function nested() {} } // nested — not a page global... but suppression-only
const {destructured} = obj;               // destructuring target — skipped
`)

	for _, name := range []string{"topFn", "oldVar", "lexical", "arrow", "attached"} {
		if !facts.declaredNames[name] {
			t.Errorf("declaredNames missing %q: %v", name, facts.declaredNames)
		}
	}
	if facts.declaredNames["nested"] {
		t.Error("nested function names are not top-level declarations")
	}
	if facts.declaredNames["destructured"] {
		t.Error("destructuring targets must be skipped")
	}
}

func TestCollectJSFacts_StringLiteralsAndTemplates(t *testing.T) {
	t.Parallel()

	facts := factsOf(t, `
const a = 'thanks.html';
const b = `+"`/orders.html`"+`;
const c = `+"`mix-${a}`"+`; // has expressions — not a plain literal
`)

	got := map[string]bool{}
	for _, s := range facts.stringLiterals {
		got[s] = true
	}
	if !got["thanks.html"] || !got["/orders.html"] {
		t.Errorf("stringLiterals missing plain literals: %v", facts.stringLiterals)
	}
	if got["mix-"] {
		t.Error("templates with expressions must not contribute partial literals")
	}
}

func TestCollectJSFacts_Gates(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name        string
		js          string
		wantDynamic bool
		wantFailed  bool
	}{
		{"static script", `document.getElementById('x').textContent = 'hi';`, false, false},
		{"innerHTML marks dynamic", `wall.innerHTML = '<p>hi</p>';`, true, false},
		{"createElement marks dynamic", `const d = document.createElement('div');`, true, false},
		{"setAttribute marks dynamic", `el.setAttribute('id', 'x');`, true, false},
		{"parse failure recorded", `this is not js {{{`, false, true},
		{"dynamic survives parse failure", `broken { innerHTML`, true, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			facts := factsOf(t, tc.js)
			if facts.dynamicDOM != tc.wantDynamic {
				t.Errorf("dynamicDOM = %v, want %v", facts.dynamicDOM, tc.wantDynamic)
			}
			if facts.parseFailed != tc.wantFailed {
				t.Errorf("parseFailed = %v, want %v", facts.parseFailed, tc.wantFailed)
			}
		})
	}
}

func TestJSFileLiterals(t *testing.T) {
	t.Parallel()

	lits := jsFileLiterals("functions/submit.js", `
module.exports = function(request) {
  return response.redirect("/thanks.html");
};`)
	found := false
	for _, l := range lits {
		if l == "/thanks.html" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the redirect literal, got %v", lits)
	}

	if got := jsFileLiterals("functions/bad.js", `not js {{{`); got != nil {
		t.Errorf("parse failure must return nil, got %v", got)
	}
}
