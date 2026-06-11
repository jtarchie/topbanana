package lint

import (
	"fmt"
	"strings"

	"github.com/dop251/goja/parser"
	"golang.org/x/net/html"
)

// checkInlineJS walks a parsed HTML tree and runs the JS parser over every
// inline <script> element. Catches syntax errors like invalid assignment LHS
// before they ship to the browser. External (`src=…`) and non-JS typed
// scripts are skipped — the agent's prompt forbids external scripts anyway,
// and JSON / importmap content isn't JS.
func checkInlineJS(filename string, doc *html.Node) []Error {
	var errs []Error
	idx := 0
	WalkDOM(doc, func(n *html.Node) {
		if n.Type != html.ElementNode || n.Data != "script" || !isLintableScript(n) {
			return
		}
		idx++
		src := scriptText(n)
		if strings.TrimSpace(src) == "" {
			return
		}
		_, err := parser.ParseFile(nil, filename, src, 0)
		if err != nil {
			errs = append(errs, Error{
				File:    filename,
				Message: fmt.Sprintf("inline <script> #%d parse error: %s", idx, err),
			})
		}
	})
	return errs
}

// isLintableScript decides whether to parse a <script>. Skipped: anything with
// a src attribute, and anything whose type isn't empty / text/javascript /
// application/javascript / module. type="module" is parsed too — goja's
// parser accepts top-level await and most module syntax, and the cost of a
// false positive there is small compared to missing a real bug.
func isLintableScript(n *html.Node) bool {
	for _, a := range n.Attr {
		switch strings.ToLower(a.Key) {
		case "src":
			if strings.TrimSpace(a.Val) != "" {
				return false
			}
		case "type":
			t := strings.ToLower(strings.TrimSpace(a.Val))
			switch t {
			case "", "text/javascript", "application/javascript", "module":
				// JS — keep walking attributes
			default:
				return false
			}
		}
	}
	return true
}

// scriptText collects the raw text inside a <script>. Children of a script
// element are TextNodes per the HTML5 parser; concatenate them.
func scriptText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return b.String()
}
