package lint

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func parseHTMLForTest(t *testing.T, src string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("html.Parse failed: %s", err)
	}
	return doc
}

func TestInlineJS_HappyPath(t *testing.T) {
	src := `<!doctype html><html><body><script>
		const x = 1;
		document.querySelector('p').textContent = String(x);
	</script></body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) != 0 {
		t.Fatalf("expected no errors, got: %v", errs)
	}
}

func TestInlineJS_CatchesInvalidAssignLHS(t *testing.T) {
	// The bug from rose-gull-89: `container *container.innerHTML = '...'`
	// is parsed as `(container * container.innerHTML) = '...'`, which is an
	// invalid assignment target. Browsers raise SyntaxError; goja agrees.
	src := `<!doctype html><html><body><script>
		var container = document.body;
		container *container.innerHTML = '<p>oops</p>';
	</script></body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) == 0 {
		t.Fatal("expected a parse error for invalid assignment LHS")
	}
	if !strings.Contains(errs[0].Message, "parse error") {
		t.Fatalf("expected 'parse error' message, got: %v", errs[0])
	}
}

func TestInlineJS_UnterminatedString(t *testing.T) {
	src := `<!doctype html><html><body><script>
		var x = "unterminated;
	</script></body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) == 0 {
		t.Fatal("expected a parse error for unterminated string")
	}
}

func TestInlineJS_SkipsExternalSrc(t *testing.T) {
	// External scripts are caught by other rules; the inline check must not
	// invent a syntax error from an empty body.
	src := `<!doctype html><html><body><script src="https://example.com/bad.js"></script></body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for src= scripts, got: %v", errs)
	}
}

func TestInlineJS_SkipsApplicationJSON(t *testing.T) {
	// <script type="application/json"> is data, not code — parsing it as JS
	// would false-positive on legitimate JSON-LD / embedded config.
	src := `<!doctype html><html><body>
		<script type="application/ld+json">{ "@context": "https://schema.org" }</script>
	</body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for application/ld+json, got: %v", errs)
	}
}

func TestInlineJS_ChecksTypeTextJavaScript(t *testing.T) {
	src := `<!doctype html><html><body>
		<script type="text/javascript">var = 1;</script>
	</body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) == 0 {
		t.Fatal("expected a parse error for type=text/javascript with bad syntax")
	}
}

func TestInlineJS_EmptyScriptIsFine(t *testing.T) {
	src := `<!doctype html><html><body><script></script><script>   </script></body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) != 0 {
		t.Fatalf("expected no errors for empty scripts, got: %v", errs)
	}
}

func TestInlineJS_MultipleScriptsIndependent(t *testing.T) {
	// First script is fine, second is broken — both walked, second reported.
	src := `<!doctype html><html><body>
		<script>console.log("hi");</script>
		<script>foo bar baz +=</script>
	</body></html>`
	errs := checkInlineJS("index.html", parseHTMLForTest(t, src))
	if len(errs) == 0 {
		t.Fatal("expected the second script to be reported")
	}
	if !strings.Contains(errs[0].Message, "#2") {
		t.Fatalf("expected error to identify script #2, got: %v", errs[0])
	}
}
