package lint

import (
	"strings"

	"golang.org/x/net/html"
)

// Unconditional HTML-AAM mappings only; context-dependent and ambiguous elements stay unmapped so the checks stand down rather than guess.
var staticImplicitRoles = map[string]string{
	"article":    "article",
	"aside":      "complementary",
	"blockquote": "blockquote",
	"button":     "button",
	"caption":    "caption",
	"code":       "code",
	"datalist":   "listbox",
	"dd":         "definition",
	"details":    "group",
	"dfn":        "term",
	"dialog":     "dialog",
	"div":        "generic",
	"dt":         "term",
	"fieldset":   "group",
	"figcaption": "caption",
	"figure":     "figure",
	"form":       "form",
	"h1":         "heading",
	"h2":         "heading",
	"h3":         "heading",
	"h4":         "heading",
	"h5":         "heading",
	"h6":         "heading",
	"hr":         "separator",
	"main":       "main",
	"menu":       "list",
	"meter":      "meter",
	"nav":        "navigation",
	"ol":         "list",
	"optgroup":   "group",
	"option":     "option",
	"output":     "status",
	"p":          "paragraph",
	"progress":   "progressbar",
	"search":     "search",
	"span":       "generic",
	"table":      "table",
	"tbody":      "rowgroup",
	"textarea":   "textbox",
	"tfoot":      "rowgroup",
	"thead":      "rowgroup",
	"time":       "time",
	"tr":         "row",
	"ul":         "list",
}

// input types whose mapping does not depend on any other attribute.
var inputTypeRoles = map[string]string{
	"button":   "button",
	"checkbox": "checkbox",
	"email":    "textbox",
	"image":    "button",
	"number":   "spinbutton",
	"radio":    "radio",
	"range":    "slider",
	"reset":    "button",
	"submit":   "button",
	"tel":      "textbox",
	"text":     "textbox",
	"url":      "textbox",
}

// An explicit role= wins over the implicit one, as in a browser.
func elementRole(n *html.Node) (string, bool) {
	// role= takes a space-separated fallback list; the first defined role wins, as in a browser.
	for _, tok := range strings.Fields(attrVal(n, "role")) {
		if ariaConcrete(tok) {
			return tok, true
		}
	}
	return implicitRole(n)
}

func implicitRole(n *html.Node) (string, bool) {
	if r, ok := staticImplicitRoles[n.Data]; ok {
		return r, true
	}
	switch n.Data {
	case "a", "area":
		return anchorRole(n)
	case "img":
		return imgRole(n)
	case "input":
		return inputRole(n)
	case "select":
		return selectRole(n)
	case "li":
		return listItemRole(n)
	case "td":
		if inTable(n) {
			return "cell", true
		}
	case "section":
		return sectionRole(n)
	}
	return "", false
}

// Without href these are not links, just generic containers.
func anchorRole(n *html.Node) (string, bool) {
	if hasAttr(n, "href") {
		return "link", true
	}
	if n.Data == "a" {
		return "generic", true
	}
	return "", false
}

// alt="" is the author declaring the image decorative, which is exactly role=presentation.
func imgRole(n *html.Node) (string, bool) {
	if v, ok := attrLookup(n, "alt"); ok && strings.TrimSpace(v) == "" {
		return "presentation", true
	}
	return "img", true
}

func selectRole(n *html.Node) (string, bool) {
	if hasAttr(n, "multiple") || sizeGreaterThanOne(n) {
		return "listbox", true
	}
	return "combobox", true
}

// Only a list parent makes it a listitem; a stray <li> maps to nothing.
func listItemRole(n *html.Node) (string, bool) {
	p := n.Parent
	if p == nil || p.Type != html.ElementNode {
		return "", false
	}
	switch p.Data {
	case "ul", "ol", "menu":
		return "listitem", true
	}
	return "", false
}

// A section is only a landmark once it has a name to announce.
func sectionRole(n *html.Node) (string, bool) {
	if hasAttr(n, "aria-label") || hasAttr(n, "aria-labelledby") {
		return "region", true
	}
	return "generic", true
}

func inputRole(n *html.Node) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(attrVal(n, "type")))
	if t == "" {
		t = "text"
	}
	if t == "hidden" {
		return "", false
	}
	// A search input becomes a combobox once it is wired to a datalist.
	if t == "search" {
		if hasAttr(n, "list") {
			return "combobox", true
		}
		return "textbox", true
	}
	if t == "text" && hasAttr(n, "list") {
		return "combobox", true
	}
	r, ok := inputTypeRoles[t]
	return r, ok
}

func sizeGreaterThanOne(n *html.Node) bool {
	v := strings.TrimSpace(attrVal(n, "size"))
	return v != "" && v != "0" && v != "1"
}

func inTable(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "table" {
			return true
		}
	}
	return false
}

func attrLookup(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}
