package lint

import (
	"fmt"
	"strings"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/lint/ariadata"
)

// An unnamed control is the most common way generated markup locks out a screen reader, and the cheapest to repair.

// Deliberately narrower than the spec's accessibleNameRequired, so container roles cannot turn an unnamed region into a failed build.
var mustNameControl = map[string]bool{
	"button":           true,
	"link":             true,
	"menuitem":         true,
	"menuitemcheckbox": true,
	"menuitemradio":    true,
	"tab":              true,
	"treeitem":         true,
	"switch":           true,
	"searchbox":        true,
	"radio":            true,
	"checkbox":         true,
	"combobox":         true,
	"listbox":          true,
	"slider":           true,
	"spinbutton":       true,
	"textbox":          true,
}

// Native controls report a missing name as a missing <label>, which is the repair their authors expect.
var fieldElements = map[string]bool{
	"input":    true,
	"select":   true,
	"textarea": true,
}

type nameIndex struct {
	byID       map[string]*html.Node
	labelFor   map[string]bool
	hasDynamic bool
}

func buildNameIndex(pi pageInfo, facts jsFacts) nameIndex {
	idx := nameIndex{
		byID:       make(map[string]*html.Node, len(pi.ids)),
		labelFor:   map[string]bool{},
		hasDynamic: facts.dynamicDOM || facts.parseFailed,
	}
	for _, n := range pi.elements {
		if id := attrVal(n, "id"); id != "" {
			if _, seen := idx.byID[id]; !seen {
				idx.byID[id] = n
			}
		}
		if n.Data == "label" {
			// Only a label with text of its own actually names anything.
			if forID := strings.TrimSpace(attrVal(n, "for")); forID != "" && visibleText(n) != "" {
				idx.labelFor[forID] = true
			}
		}
	}
	return idx
}

// checkAccessibleNames runs the alt-text, control-name, and field-label rules over one page.
func checkAccessibleNames(pi pageInfo, facts jsFacts) []Error {
	idx := buildNameIndex(pi, facts)
	var errs []Error
	for _, n := range pi.elements {
		if hiddenFromAT(n) {
			continue
		}
		errs = append(errs, checkImageAlt(pi, n)...)
		errs = append(errs, checkControlName(pi, n, idx)...)
	}
	return errs
}

// alt="" is a valid decorative declaration; a missing attribute means the file name gets announced instead.
func checkImageAlt(pi pageInfo, n *html.Node) []Error {
	isImage := n.Data == "img" || (n.Data == "input" && strings.EqualFold(attrVal(n, "type"), "image"))
	if !isImage {
		return nil
	}
	if _, ok := attrLookup(n, "alt"); ok {
		return nil
	}
	if hasAttr(n, "aria-label") || hasAttr(n, "aria-labelledby") {
		return nil
	}
	src := attrVal(n, "src")
	if src == "" {
		src = "(no src)"
	}
	return []Error{{
		File: pi.name, Kind: KindMissingAlt,
		Message: fmt.Sprintf(
			`missing alt text — <%s src=%q> has no alt attribute, so a screen reader announces the file name instead of the image. Add alt="..." describing what the image conveys, or alt="" if it is purely decorative and should be skipped.`,
			n.Data, src),
	}}
}

func checkControlName(pi pageInfo, n *html.Node, idx nameIndex) []Error {
	// <datalist> maps to role=listbox but is never rendered or announced — it
	// is the suggestion source for the input that names it, so demanding a
	// name of its own would fail every build that uses one.
	if n.Data == "datalist" {
		return nil
	}
	role, ok := elementRole(n)
	if !ok || !mustNameControl[role] {
		return nil
	}
	if hasName(n, idx, role) {
		return nil
	}
	// An empty control on a script-built page is plausibly filled at runtime; an unlabeled icon is not.
	if idx.hasDynamic && visibleText(n) == "" && n.FirstChild == nil {
		return nil
	}

	if fieldElements[n.Data] {
		return []Error{{
			File: pi.name, Kind: KindMissingFieldLabel,
			Message: fmt.Sprintf(
				`unlabeled form field — <%s%s> has no <label>, aria-label, or title, so a screen reader announces it as a bare %q and the visitor cannot tell what to type. Add <label for="ID">…</label> with a matching id, or aria-label="..." on the field. A placeholder is not a label: it disappears as soon as anyone types.`,
				n.Data, describeType(n), role),
		}}
	}
	return []Error{{
		File: pi.name, Kind: KindMissingControlName,
		Message: fmt.Sprintf(
			`unnamed control — <%s>%s carries role %q but exposes no name at all: no text, no aria-label, no labelled image inside. A screen reader announces only %q, so the visitor cannot tell what it does. Add visible text, or aria-label="..." describing the action.`,
			n.Data, describeTarget(n), role, role),
	}}
}

// Presence of a name source, not the computed name: the checks only ask whether anything names the control.
func hasName(n *html.Node, idx nameIndex, role string) bool {
	switch {
	case hasAuthoredName(n, idx):
		return true
	case fieldElements[n.Data] && hasLabelAssociation(n, idx):
		return true
	case n.Data == "input":
		return inputCarriesName(n)
	case n.Data == "select" || n.Data == "textarea":
		return false
	}
	if def, ok := ariadata.Get(role); ok && def.NameFromContents {
		return namedByContents(n)
	}
	return false
}

func hasAuthoredName(n *html.Node, idx nameIndex) bool {
	if strings.TrimSpace(attrVal(n, "aria-label")) != "" {
		return true
	}
	if strings.TrimSpace(attrVal(n, "title")) != "" {
		return true
	}
	refs := strings.Fields(attrVal(n, "aria-labelledby"))
	if len(refs) == 0 {
		return false
	}
	for _, id := range refs {
		if target, ok := idx.byID[id]; ok && visibleText(target) != "" {
			return true
		}
	}
	// A page that writes its own markup may name an element that is not in the source yet.
	return idx.hasDynamic
}

func hasLabelAssociation(n *html.Node, idx nameIndex) bool {
	if id := attrVal(n, "id"); id != "" && idx.labelFor[id] {
		return true
	}
	return inLabelWithText(n)
}

// submit and reset carry a default label even with no value attribute.
func inputCarriesName(n *html.Node) bool {
	t := strings.ToLower(strings.TrimSpace(attrVal(n, "type")))
	switch t {
	case "submit", "reset":
		return true
	case "button":
		return strings.TrimSpace(attrVal(n, "value")) != ""
	case "image":
		return strings.TrimSpace(attrVal(n, "alt")) != ""
	}
	return false
}

// namedByContents covers the three ways a control's own subtree supplies a name.
func namedByContents(n *html.Node) bool {
	if visibleText(n) != "" {
		return true
	}
	named := false
	WalkDOM(n, func(c *html.Node) {
		if named || c == n || c.Type != html.ElementNode || hiddenFromAT(c) {
			return
		}
		switch {
		case c.Data == "img" && strings.TrimSpace(attrVal(c, "alt")) != "":
			named = true
		case c.Data == "svg" && svgTitle(c) != "":
			named = true
		case strings.TrimSpace(attrVal(c, "aria-label")) != "":
			named = true
		case hasAttr(c, "aria-labelledby"):
			named = true
		}
	})
	return named
}

func svgTitle(svg *html.Node) string {
	title := ""
	WalkDOM(svg, func(c *html.Node) {
		if title == "" && c.Type == html.ElementNode && c.Data == "title" {
			title = visibleText(c)
		}
	})
	return title
}

func inLabelWithText(n *html.Node) bool {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && p.Data == "label" {
			return visibleText(p) != ""
		}
	}
	return false
}

// hiddenFromAT reports elements assistive technology never reaches, which nothing here should judge.
func hiddenFromAT(n *html.Node) bool {
	if strings.EqualFold(strings.TrimSpace(attrVal(n, "aria-hidden")), "true") {
		return true
	}
	if hasAttr(n, "hidden") {
		return true
	}
	if n.Data == "input" && strings.EqualFold(attrVal(n, "type"), "hidden") {
		return true
	}
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type != html.ElementNode {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(attrVal(p, "aria-hidden")), "true") || hasAttr(p, "hidden") {
			return true
		}
	}
	return false
}

// What a screen reader would read: aria-hidden and non-rendered subtrees contribute nothing, and &nbsp;-only content is empty.
func visibleText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(c *html.Node) {
		switch {
		case c.Type == html.TextNode:
			b.WriteString(c.Data)
			return
		case c.Type != html.ElementNode:
			return
		}
		switch c.Data {
		case "script", "style", "template", "noscript":
			return
		}
		if c != n && strings.EqualFold(strings.TrimSpace(attrVal(c, "aria-hidden")), "true") {
			return
		}
		for child := c.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	return strings.TrimSpace(strings.ReplaceAll(b.String(), " ", " "))
}

func describeType(n *html.Node) string {
	if t := strings.TrimSpace(attrVal(n, "type")); t != "" && n.Data == "input" {
		return fmt.Sprintf(" type=%q", t)
	}
	if name := strings.TrimSpace(attrVal(n, "name")); name != "" {
		return fmt.Sprintf(" name=%q", name)
	}
	return ""
}

func describeTarget(n *html.Node) string {
	if href := strings.TrimSpace(attrVal(n, "href")); href != "" {
		return fmt.Sprintf(" pointing at %q", href)
	}
	if id := strings.TrimSpace(attrVal(n, "id")); id != "" {
		return fmt.Sprintf(" with id %q", id)
	}
	return ""
}
