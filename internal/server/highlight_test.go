package server

import (
	"strings"
	"testing"
)

// The function editor renders this straight into the page, so the two things
// that matter are that it tokenises at all and that source text stays escaped.
func TestHighlightJS(t *testing.T) {
	t.Parallel()

	got := string(highlightJS("const s = '<script>alert(1)</script>';\n"))
	if !strings.Contains(got, `<span class="k`) {
		t.Errorf("no keyword token in output: %q", got)
	}
	if strings.Contains(got, "<script>") {
		t.Errorf("source markup left unescaped: %q", got)
	}
	if !strings.Contains(got, "&lt;script&gt;") {
		t.Errorf("escaped source missing: %q", got)
	}
	if css := string(highlightCSS()); !strings.Contains(css, ".chroma .k") {
		t.Errorf("stylesheet missing keyword rule: %q", css)
	}
}
