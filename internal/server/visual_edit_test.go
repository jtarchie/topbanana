package server

import (
	"bytes"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// renderPretty runs the document fragment through prettyPrintBlockElements
// + html.Render. Used by tests that want to assert on the post-format
// serialization without going through assemblePage.
func renderPretty(t *testing.T, src string) string {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	prettyPrintBlockElements(doc, -1)
	var buf bytes.Buffer
	err = html.Render(&buf, doc)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func TestPrettyPrintBlockElements_BodyChildrenOnSeparateLines(t *testing.T) {
	// GrapesJS-style compact body: no whitespace between siblings.
	src := `<!DOCTYPE html><html><head><title>t</title></head><body><header><h1>Hi</h1></header><main><section><h2>Hours</h2></section><section><h2>Menu</h2></section></main></body></html>`
	got := renderPretty(t, src)

	// Body siblings (header, main) must be separated by a newline.
	if !strings.Contains(got, "</header>\n") || !strings.Contains(got, "<main>") {
		t.Errorf("body children should be on separate lines; got:\n%s", got)
	}
	// main's section siblings too.
	if strings.Count(got, "<section>") != 2 {
		t.Errorf("expected two <section> tags; got:\n%s", got)
	}
	if !strings.Contains(got, "</section>\n") {
		t.Errorf("section siblings should be newline-separated; got:\n%s", got)
	}
}

func TestPrettyPrintBlockElements_Idempotent(t *testing.T) {
	src := `<!DOCTYPE html><html><head><title>t</title></head><body><header><h1>Hi</h1></header><main><section>A</section><section>B</section></main></body></html>`
	once := renderPretty(t, src)
	twice := renderPretty(t, once)
	if once != twice {
		t.Errorf("formatting should be idempotent.\nfirst pass:\n%s\nsecond pass:\n%s", once, twice)
	}
}

func TestPrettyPrintBlockElements_PreservesPreContent(t *testing.T) {
	// Whitespace inside <pre> is significant — must not be touched.
	preBody := "line1\n  line2\n    line3"
	src := `<!DOCTYPE html><html><head></head><body><pre>` + preBody + `</pre></body></html>`
	got := renderPretty(t, src)
	if !strings.Contains(got, preBody) {
		t.Errorf("<pre> content was modified.\nwant substring: %q\ngot:\n%s", preBody, got)
	}
}

func TestPrettyPrintBlockElements_PreservesScriptContent(t *testing.T) {
	script := "var x = 1;\nvar y = 2;"
	src := `<!DOCTYPE html><html><head><script>` + script + `</script></head><body></body></html>`
	got := renderPretty(t, src)
	if !strings.Contains(got, script) {
		t.Errorf("<script> content was modified.\nwant substring: %q\ngot:\n%s", script, got)
	}
}

func TestPrettyPrintBlockElements_PreservesInlineSiblings(t *testing.T) {
	// Inline siblings inside a <button> or <p> must not get whitespace
	// injected between them — that would introduce visible spaces.
	src := `<!DOCTYPE html><html><head></head><body><button><span>A</span><span>B</span></button><p>hello <b>world</b></p></body></html>`
	got := renderPretty(t, src)

	if !strings.Contains(got, `<span>A</span><span>B</span>`) {
		t.Errorf("inline button children should stay adjacent; got:\n%s", got)
	}
	if !strings.Contains(got, `<p>hello <b>world</b></p>`) {
		t.Errorf("mixed-content paragraph should stay byte-identical; got:\n%s", got)
	}
}

func TestPrettyPrintBlockElements_EmptyContainerStaysCollapsed(t *testing.T) {
	src := `<!DOCTYPE html><html><head></head><body><div></div></body></html>`
	got := renderPretty(t, src)
	if !strings.Contains(got, `<div></div>`) {
		t.Errorf("empty <div> should stay collapsed; got:\n%s", got)
	}
}

func TestAssemblePage_BodyOnePerLine(t *testing.T) {
	original := `<!DOCTYPE html>
<html>
<head><title>t</title><style>body{color:red}</style></head>
<body></body>
</html>`
	// Mimic GrapesJS output: compact, no inter-tag whitespace.
	newHTML := `<header><h1>Hi</h1></header><main><section class="hours"><h2>Hours</h2></section><section><h2>Menu</h2></section></main>`
	out, err := assemblePage(original, newHTML, "body{color:blue}")
	if err != nil {
		t.Fatalf("assemblePage: %v", err)
	}

	// Body children must be on separate lines so replace_lines can target them.
	if !strings.Contains(out, "</header>\n") {
		t.Errorf("header should be followed by a newline. Output:\n%s", out)
	}
	if !strings.Contains(out, "</section>\n") {
		t.Errorf("section siblings should be newline-separated. Output:\n%s", out)
	}
	// CSS body got swapped.
	if !strings.Contains(out, "body{color:blue}") {
		t.Errorf("new CSS missing. Output:\n%s", out)
	}
}

func TestAssemblePage_Idempotent(t *testing.T) {
	original := `<!DOCTYPE html><html><head><style>x{}</style></head><body></body></html>`
	newHTML := `<header><h1>Hi</h1></header><main><section>A</section><section>B</section></main>`
	once, err := assemblePage(original, newHTML, "x{}")
	if err != nil {
		t.Fatalf("first assemblePage: %v", err)
	}
	// Feed the output of one save back in as if the user saved a second time
	// without changing anything. We extract the body & CSS from `once`'s tree.
	parts, err := splitPage(once)
	if err != nil {
		t.Fatalf("splitPage: %v", err)
	}
	twice, err := assemblePage(once, parts.BodyHTML, parts.CSS)
	if err != nil {
		t.Fatalf("second assemblePage: %v", err)
	}
	if once != twice {
		t.Errorf("assemblePage should be idempotent across consecutive saves.\nfirst:\n%s\nsecond:\n%s", once, twice)
	}
}
