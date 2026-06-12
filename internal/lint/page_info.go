package lint

import (
	"path"
	"strings"

	"github.com/dop251/goja/ast"
	"github.com/dop251/goja/parser"
	"golang.org/x/net/html"
)

// pageInfo is the per-page collected model: one WalkDOM pass gathers
// everything the per-page and cross-page checks need, so adding a check costs
// a loop over a slice rather than another tree traversal.
type pageInfo struct {
	name     string
	dir      string // path.Dir(name), what link resolution needs
	doc      *html.Node
	elements []*html.Node   // every ElementNode, document order
	ids      map[string]int // id attribute value → occurrence count
	targets  map[string]bool
	scripts  []scriptInfo
}

// scriptInfo is one non-empty lintable inline <script>: its ordinal (counting
// every lintable script, empty ones included, so checkInlineJS's "#%d"
// numbering matches what an author sees in the file), its raw text, and the
// parse result. program is nil when the parse failed — checkInlineJS reports
// parseErr once and downstream JS checks treat the page as unanalyzable.
type scriptInfo struct {
	ordinal  int
	text     string
	program  *ast.Program
	parseErr error
}

// collectPageInfo does the single walk over a parsed page. targets gets every
// element's id plus the legacy name attribute on <a> — the fragment targets a
// page exposes (ids anywhere also cover inline-SVG <use href="#icon">
// symbols). Note: ids inside <template> are collected too even though
// browsers' getElementById can't see them — a false-negative direction the
// id-based checks accept.
func collectPageInfo(name string, doc *html.Node) pageInfo {
	pi := pageInfo{
		name:    name,
		dir:     path.Dir(name),
		doc:     doc,
		ids:     map[string]int{},
		targets: map[string]bool{},
	}
	scriptCount := 0
	WalkDOM(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		pi.elements = append(pi.elements, n)
		for _, attr := range n.Attr {
			switch {
			case attr.Key == "id" && attr.Val != "":
				pi.ids[attr.Val]++
				pi.targets[attr.Val] = true
			case attr.Key == "name" && n.Data == "a" && attr.Val != "":
				pi.targets[attr.Val] = true
			}
		}
		if n.Data == "script" && isLintableScript(n) {
			scriptCount++
			text := scriptText(n)
			if strings.TrimSpace(text) == "" {
				return
			}
			si := scriptInfo{ordinal: scriptCount, text: text}
			si.program, si.parseErr = parser.ParseFile(nil, name, text, 0)
			if si.parseErr != nil {
				si.program = nil
			}
			pi.scripts = append(pi.scripts, si)
		}
	})
	return pi
}
