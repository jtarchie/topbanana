package lint

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"
)

// checkInlineJS reports parse errors for a page's inline <script> elements.
// Catches syntax errors like invalid assignment LHS before they ship to the
// browser. The scripts arrive pre-parsed via collectPageInfo (which applies
// the isLintableScript filter — external `src=…` and non-JS typed scripts
// are skipped; the agent's prompt forbids external scripts anyway, and
// JSON / importmap content isn't JS).
func checkInlineJS(filename string, scripts []scriptInfo) []Error {
	var errs []Error
	for _, s := range scripts {
		if s.parseErr != nil {
			errs = append(errs, Error{
				File:    filename,
				Message: fmt.Sprintf("inline <script> #%d parse error: %s", s.ordinal, s.parseErr),
			})
		}
	}
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
