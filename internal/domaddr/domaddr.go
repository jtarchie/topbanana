// Package domaddr gives every element in a stored HTML document a stable
// address: its index in tokenizer document order. The canvas editor's proxy
// stamps that index onto each element as data-tb-el, the browser reports it
// back on selection, and Resolve maps it to the exact byte span in the stored
// source — so a selection identifies an *element*, never "the text that says
// X", which breaks the moment content is duplicated on a page.
//
// The one invariant everything rests on: Annotate and Resolve run the same
// walk over the same bytes, so index N means the same element to both. The
// browser's own DOM order is irrelevant — it just echoes the attribute value
// it was served.
package domaddr

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"

	"golang.org/x/net/html"
)

// voidElements never take a closing tag; their span is the start tag alone
// and they don't deepen the nesting count.
var voidElements = map[string]bool{
	"area": true, "base": true, "br": true, "col": true, "embed": true,
	"hr": true, "img": true, "input": true, "link": true, "meta": true,
	"source": true, "track": true, "wbr": true,
}

// Span is the byte range of one element in the source document, from the
// first byte of its start tag through the last byte of its end tag (or the
// start tag alone for void/self-closing elements, or EOF when the source
// never closes it).
type Span struct {
	Start int
	End   int
}

// walk tokenizes doc, calling visit for every element start with its index,
// tag name, and the token's byte range. copyTo, when non-nil, receives the
// raw bytes of every token — visit may return replacement bytes for the
// element's start tag (nil keeps the original). This single function backs
// both Annotate and Resolve so their orderings can never diverge.
func walk(doc []byte, visit func(index int, name string, tokStart, tokEnd int, raw []byte) []byte, copyTo *bytes.Buffer) {
	z := html.NewTokenizer(bytes.NewReader(doc))
	offset := 0
	index := 0
	for {
		tt := z.Next()
		raw := z.Raw()
		tokStart := offset
		offset += len(raw)
		if tt == html.ErrorToken {
			// EOF or a tokenize error; either way the remaining bytes are
			// carried through verbatim so Annotate never drops content.
			if copyTo != nil {
				copyTo.Write(raw)
			}
			return
		}
		if tt == html.StartTagToken || tt == html.SelfClosingTagToken {
			name, _ := z.TagName()
			out := visit(index, string(name), tokStart, offset, raw)
			index++
			if copyTo != nil {
				if out != nil {
					copyTo.Write(out)
				} else {
					copyTo.Write(raw)
				}
			}
			continue
		}
		if copyTo != nil {
			copyTo.Write(raw)
		}
	}
}

// Annotate returns doc with data-tb-el="N" inserted into every element start
// tag, N counting elements in document order from 0. Everything else —
// whitespace, attribute quoting, text, comments — passes through verbatim.
func Annotate(doc []byte) []byte {
	return AnnotateAndRebase(doc, "")
}

// rootURLAttr matches a root-absolute URL value in the reference attributes
// of a single start tag ("/x" but not protocol-relative "//x"). Applied only
// to tag bytes during the walk, never to text or script content, so an
// inline script mentioning src="/api/…" in a string stays untouched.
var rootURLAttr = regexp.MustCompile(`(\s(?:href|src|action|poster)=["'])(/[^/"'])`)

// AnnotateAndRebase is Annotate plus URL rebasing for pages served off a
// path mount: a site's root-absolute references (href="/site.css",
// src="/assets/x.png") resolve against the admin origin's root when the page
// renders inside the /s/:slug iframe, so edit mode rewrites them onto the
// mount prefix. Relative URLs already resolve correctly and are left alone;
// so are protocol-relative and absolute-origin URLs. url(...) inside
// stylesheets is not rewritten — the sites the platform builds don't use
// root-absolute url() references.
//
// ponytail: srcset attributes are not rebased (comma-separated multi-URL
// values need their own parser); add when a generated site first uses one.
func AnnotateAndRebase(doc []byte, prefix string) []byte {
	var out bytes.Buffer
	out.Grow(len(doc) + 16*bytes.Count(doc, []byte("<")))
	walk(doc, func(index int, name string, _, _ int, raw []byte) []byte {
		tag := injectAttr(raw, index)
		if prefix != "" {
			tag = rootURLAttr.ReplaceAll(tag, []byte("${1}"+prefix+"${2}"))
		}
		return tag
	}, &out)
	return out.Bytes()
}

// injectAttr splices ` data-tb-el="n"` into a raw start tag immediately after
// the tag name. Works on the original bytes so the rest of the tag (attribute
// order, quoting, case) is untouched.
func injectAttr(raw []byte, n int) []byte {
	// raw is `<name ...>` — find the end of the name: first byte after `<`
	// that isn't part of a tag name. Tag names can't contain space, /, or >.
	i := 1
	for i < len(raw) && raw[i] != ' ' && raw[i] != '\t' && raw[i] != '\n' && raw[i] != '\r' && raw[i] != '/' && raw[i] != '>' {
		i++
	}
	var b bytes.Buffer
	b.Grow(len(raw) + 24)
	b.Write(raw[:i])
	b.WriteString(` data-tb-el="`)
	b.WriteString(strconv.Itoa(n))
	b.WriteString(`"`)
	b.Write(raw[i:])
	return b.Bytes()
}

// Count returns how many elements the document contains — the exclusive upper
// bound on valid addresses.
func Count(doc []byte) int {
	n := 0
	walk(doc, func(int, string, int, int, []byte) []byte { n++; return nil }, nil)
	return n
}

// impliedClosers maps an open element to the start tags that implicitly
// close it — the HTML optional-end-tag rules a browser applies while
// parsing. Checked against the top of the open-element stack only, which is
// what makes nesting work: an inner <li> under an open <ul> can't close the
// outer <li> two levels up.
var impliedClosers = map[string]map[string]bool{
	"li":       {"li": true},
	"dt":       {"dt": true, "dd": true},
	"dd":       {"dt": true, "dd": true},
	"option":   {"option": true, "optgroup": true},
	"optgroup": {"optgroup": true},
	"tr":       {"tr": true, "tbody": true, "tfoot": true, "thead": true},
	"td":       {"td": true, "th": true, "tr": true, "tbody": true, "tfoot": true, "thead": true},
	"th":       {"td": true, "th": true, "tr": true, "tbody": true, "tfoot": true, "thead": true},
	"thead":    {"tbody": true, "tfoot": true},
	"tbody":    {"tbody": true, "tfoot": true},
	"caption":  {"tr": true, "tbody": true, "tfoot": true, "thead": true, "colgroup": true},
	"colgroup": {"tr": true, "tbody": true, "tfoot": true, "thead": true, "caption": true},
	"p": {
		"address": true, "article": true, "aside": true, "blockquote": true,
		"details": true, "div": true, "dl": true, "fieldset": true,
		"figcaption": true, "figure": true, "footer": true, "form": true,
		"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
		"header": true, "hgroup": true, "hr": true, "main": true, "menu": true,
		"nav": true, "ol": true, "p": true, "pre": true, "section": true,
		"table": true, "ul": true,
	},
}

// openStack tracks the elements open inside a target element and applies the
// browser's optional-end-tag rules. The target itself is the bottom entry;
// when the stack empties the target has closed.
type openStack struct{ names []string }

func (s *openStack) depth() int { return len(s.names) }

// openTag processes a start tag inside the target: implied end tags first (a
// second <li> closes the first; a <div> closes an open <p> — top-of-stack
// only, which is what keeps nesting correct), then the push. Reports whether
// the implied closes reached the target itself.
func (s *openStack) openTag(name string, void bool) (closedTarget bool) {
	for len(s.names) > 0 && impliedClosers[s.names[len(s.names)-1]][name] {
		s.names = s.names[:len(s.names)-1]
	}
	if len(s.names) == 0 {
		return true
	}
	if !void {
		s.names = append(s.names, name)
	}
	return false
}

// closeTag processes an end tag: void end tags (</br>) and end tags matching
// nothing open are ignored, as in a browser; a match pops through any
// implicitly-closable elements above it. Reports whether the target closed.
func (s *openStack) closeTag(name string) (closedTarget bool) {
	if voidElements[name] {
		return false
	}
	for i := len(s.names) - 1; i >= 0; i-- {
		if s.names[i] == name {
			s.names = s.names[:i]
			return len(s.names) == 0
		}
	}
	return false
}

// Resolve returns the byte span of element n in doc: from its start tag
// through the token that closes it. Closure follows the browser's rules via
// openStack, not bare tag counting. An element the source never closes spans
// to EOF: a deterministic answer for malformed HTML, since the browser
// rendered *something* for it either way. An element closed *implicitly*
// (a sibling start tag ended it) ends just before the token that closed it,
// so the span never swallows the sibling.
func Resolve(doc []byte, n int) (Span, error) {
	if n < 0 {
		return Span{}, fmt.Errorf("domaddr: negative address %d", n)
	}
	span := Span{Start: -1, End: -1}
	var stack openStack
	found := false
	z := html.NewTokenizer(bytes.NewReader(doc))
	offset := 0
	index := 0
	for {
		tt := z.Next()
		raw := z.Raw()
		tokStart := offset
		offset += len(raw)
		if tt == html.ErrorToken {
			break
		}
		switch tt { //nolint:exhaustive // text/comment/doctype tokens carry no element structure.
		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, _ := z.TagName()
			name := string(nameB)
			void := voidElements[name] || tt == html.SelfClosingTagToken
			switch {
			case index == n:
				found = true
				span.Start = tokStart
				if void {
					span.End = offset
					return span, nil
				}
				stack.names = append(stack.names, name)
			case found && stack.openTag(name, void):
				span.End = tokStart
				return span, nil
			}
			index++
		case html.EndTagToken:
			if !found {
				continue
			}
			nameB, _ := z.TagName()
			if stack.closeTag(string(nameB)) {
				span.End = offset
				return span, nil
			}
		}
	}
	if !found {
		return Span{}, fmt.Errorf("domaddr: address %d out of range (document has %d elements)", n, index)
	}
	span.End = len(doc)
	return span, nil
}

// OuterHTML returns the source bytes of element n.
func OuterHTML(doc []byte, n int) ([]byte, error) {
	span, err := Resolve(doc, n)
	if err != nil {
		return nil, err
	}
	return doc[span.Start:span.End], nil
}

// TextSpan returns the byte span of the textIndex-th direct text node of
// element n — "direct" meaning at depth 1 inside the element, matching how a
// browser counts the element's childNodes text nodes (whitespace-only runs
// included, text inside child elements excluded). One contiguous text run is
// one node on both sides, which is what lets the canvas's in-place editor
// name a text node by (element address, child text index) and have the server
// find the same bytes.
func TextSpan(doc []byte, n, textIndex int) (Span, error) {
	if textIndex < 0 {
		return Span{}, fmt.Errorf("domaddr: negative text index %d", textIndex)
	}
	elem, err := Resolve(doc, n)
	if err != nil {
		return Span{}, err
	}

	w := &textScan{target: n}
	z := html.NewTokenizer(bytes.NewReader(doc))
	offset := 0
	for {
		tt := z.Next()
		raw := z.Raw()
		tokStart := offset
		offset += len(raw)
		if tt == html.ErrorToken || tokStart >= elem.End {
			break
		}
		switch tt { //nolint:exhaustive // comments/doctype are neither structure nor editable text.
		case html.StartTagToken, html.SelfClosingTagToken:
			nameB, _ := z.TagName()
			err = w.startTag(string(nameB), tt == html.SelfClosingTagToken)
			if err != nil {
				return Span{}, err
			}
		case html.EndTagToken:
			nameB, _ := z.TagName()
			w.endTag(string(nameB))
		case html.TextToken:
			if w.directText() {
				if w.seen == textIndex {
					return Span{Start: tokStart, End: offset}, nil
				}
				w.seen++
			}
		}
	}
	return Span{}, fmt.Errorf("domaddr: element %d has no direct text node %d (found %d)", n, textIndex, w.seen)
}

// textScan is TextSpan's cursor: which element indexes have passed, whether
// the scan is inside the target, and how many direct text nodes it has seen.
type textScan struct {
	stack  openStack
	target int
	index  int
	inside bool
	seen   int
}

func (w *textScan) startTag(name string, selfClosing bool) error {
	void := voidElements[name] || selfClosing
	switch {
	case w.index == w.target:
		if void {
			return fmt.Errorf("domaddr: element %d is a void element with no text", w.target)
		}
		w.inside = true
		w.stack.names = append(w.stack.names, name)
	case w.inside && w.stack.openTag(name, void):
		w.inside = false
	}
	w.index++
	return nil
}

func (w *textScan) endTag(name string) {
	if w.inside && w.stack.closeTag(name) {
		w.inside = false
	}
}

// directText reports whether a text token at the current position is a
// direct child of the target — inside it, no intervening open element.
func (w *textScan) directText() bool { return w.inside && w.stack.depth() == 1 }
