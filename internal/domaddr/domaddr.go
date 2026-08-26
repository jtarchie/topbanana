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
	var out bytes.Buffer
	out.Grow(len(doc) + 16*bytes.Count(doc, []byte("<")))
	walk(doc, func(index int, name string, _, _ int, raw []byte) []byte {
		return injectAttr(raw, index)
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

// Resolve returns the byte span of element n in doc. The end of a non-void
// element is the matching end tag by source nesting: every non-void start tag
// deepens, every end tag shallows, and the tag that returns the depth to zero
// closes the element. Documents that never close it span to EOF — a
// deterministic answer for malformed HTML rather than an error, since the
// browser will have rendered *something* for the element either way.
func Resolve(doc []byte, n int) (Span, error) {
	if n < 0 {
		return Span{}, fmt.Errorf("domaddr: negative address %d", n)
	}
	span := Span{Start: -1, End: -1}
	depth := 0
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
			name, _ := z.TagName()
			void := voidElements[string(name)] || tt == html.SelfClosingTagToken
			if index == n {
				found = true
				span.Start = tokStart
				if void {
					span.End = offset
					return span, nil
				}
				depth = 1
			} else if found && !void {
				depth++
			}
			index++
		case html.EndTagToken:
			if found {
				depth--
				if depth == 0 {
					span.End = offset
					return span, nil
				}
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
