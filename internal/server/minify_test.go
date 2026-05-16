package server

import (
	"strings"
	"testing"
)

// TestMinifyHTMLBody_StripsWhitespace pins the basic value prop: a verbose
// agent-generated page comes back smaller while keeping the structural
// tags we configured to preserve.
func TestMinifyHTMLBody_StripsWhitespace(t *testing.T) {
	t.Parallel()

	m := newHTMLMinifier()
	in := `<!DOCTYPE html>
<html lang="en" data-theme="light">
<head>
    <meta charset="UTF-8">
    <title>Hi</title>
    <!-- a comment that should be stripped -->
</head>
<body>
    <h1>Hello</h1>
    <p>     leading spaces     </p>
</body>
</html>`

	out, err := minifyHTMLBody(m, in)
	if err != nil {
		t.Fatalf("minifyHTMLBody: %v", err)
	}
	if len(out) >= len(in) {
		t.Fatalf("output not smaller: in=%d out=%d", len(in), len(out))
	}
	// Structural tags preserved (KeepDocumentTags + KeepEndTags).
	for _, want := range []string{"<html", "<head>", "<body>", "</body>", "</html>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	// Comments stripped.
	if strings.Contains(out, "a comment") {
		t.Fatalf("comment leaked through:\n%s", out)
	}
}

// TestMinifyHTMLBody_FallsBackOnGarbage confirms a minifier hiccup never
// blanks the response. Even on input that confuses the parser, we return
// the original bytes (and a non-nil error the caller can log).
func TestMinifyHTMLBody_FallsBackOnGarbage(t *testing.T) {
	t.Parallel()

	m := newHTMLMinifier()
	in := "<!DOCTYPE html><html><<<not really html>>>"
	out, _ := minifyHTMLBody(m, in)
	// We never want an empty body back regardless of minifier behavior.
	if out == "" {
		t.Fatal("minifyHTMLBody returned empty body on suspect input")
	}
}
