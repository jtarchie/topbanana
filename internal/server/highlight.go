package server

import (
	"bytes"
	"html/template"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
)

// Source highlighting for the function editor. Class-based rather than inline
// styles so the palette ships once per page instead of once per token, and so
// the block can be restyled without re-rendering. github-dark is fixed on
// purpose: the code block is dark regardless of the admin daisyUI theme, so a
// theme-following palette would have to be contrast-checked against all of them.
var (
	chromaStyle     = styles.Get("github-dark")
	chromaFormatter = chromahtml.New(
		chromahtml.WithClasses(true),
		chromahtml.WithLineNumbers(true),
		chromahtml.LineNumbersInTable(true),
	)
	// Resolved once: lexers.Get falls through to a filename Match() on a miss,
	// and answers nil rather than a fallback lexer — Coalesce would then panic
	// on the nil interface instead of returning the error highlightJS expects.
	chromaJS = func() chroma.Lexer {
		lexer := lexers.Get("javascript")
		if lexer == nil {
			lexer = lexers.Fallback
		}
		return chroma.Coalesce(lexer)
	}()
	// The stylesheet is a pure function of a compiled-in style, so it is built
	// once here rather than memoised behind a request-time error path.
	chromaCSS = func() template.CSS {
		var buf bytes.Buffer
		err := chromaFormatter.WriteCSS(&buf, chromaStyle)
		if err != nil {
			return ""
		}
		return template.CSS(buf.String()) //nolint:gosec // generated from a compiled-in style
	}()
)

// highlightJS renders JavaScript as a complete <pre class="chroma"> block. A
// lexer or format failure falls back to the escaped source: a syntax-highlight
// nicety must never cost the author sight of their own code.
func highlightJS(src string) template.HTML {
	iterator, err := chromaJS.Tokenise(nil, src)
	if err != nil {
		return plainSource(src)
	}
	var buf bytes.Buffer
	err = chromaFormatter.Format(&buf, chromaStyle, iterator)
	if err != nil {
		return plainSource(src)
	}
	return template.HTML(buf.String()) //nolint:gosec // chroma escapes token text
}

func plainSource(src string) template.HTML {
	var buf bytes.Buffer
	template.HTMLEscape(&buf, []byte(src))
	return template.HTML("<pre class=\"chroma\"><code>" + buf.String() + "</code></pre>") //nolint:gosec // escaped above
}

// highlightCSS is the stylesheet for the classes highlightJS emits.
func highlightCSS() template.CSS { return chromaCSS }
