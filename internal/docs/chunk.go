package docs

import (
	"regexp"
	"strings"
)

const (
	// componentLevel is the deepest heading that starts its own chunk. daisyUI
	// keys each component at "###" and its class list at "#### Class names";
	// folding the deeper headings into the parent chunk keeps a component's
	// classes travelling with its description as one self-contained unit.
	componentLevel = 3
	// maxChunkSourceBytes caps a single section's body at parse time so one
	// runaway section can't dominate a result's byte budget. Generous — real
	// daisyUI component sections are an order of magnitude smaller.
	maxChunkSourceBytes = 8 * 1024
)

// Chunk is one searchable unit: a heading and the body beneath it, down to the
// next heading at the same or shallower level. headingTok/bodyTok are the
// precomputed lowercase tokens the ranker scores against (heading tokens carry
// a weight boost, so a query equal to a component name lands that component).
type Chunk struct {
	Source     string
	SourceID   string
	Breadcrumb string // "daisyUI > components > button"
	Heading    string // "button"
	Level      int    // 1..componentLevel
	Body       string // raw markdown under the heading, trimmed

	// componentClass is the daisyUI component class this section defines, read
	// from its "- component: `btn`" line (empty for non-component sections and
	// for any future non-daisyUI source). The ranker uses it to prefer the
	// section that *defines* a class family over one that merely uses it.
	componentClass string

	headingTok []string
	bodyTok    []string
}

var (
	headingRe = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*$`)
	// componentClassRe matches daisyUI's "- component: `btn`" declaration line.
	componentClassRe = regexp.MustCompile("(?m)^-\\s*component:\\s*`([a-z][a-z0-9-]*)`")
)

// chunkSource parses one source's markdown into chunks in a single linear pass,
// maintaining a stack of ancestor headings (for the breadcrumb) and a fenced-
// code-block flag so a "#" inside a ``` block is body text, not a heading. A
// leading YAML "---" frontmatter block is skipped. Headings at level
// <= componentLevel start a chunk; deeper headings fold into the current body.
func chunkSource(src sourceDef) []Chunk {
	lines := strings.Split(stripFrontmatter(src.Body), "\n")

	type stackEntry struct {
		level int
		text  string
	}

	var (
		chunks  []Chunk
		stack   []stackEntry
		cur     *Chunk
		buf     []string
		inFence bool
	)

	flush := func() {
		if cur == nil {
			buf = nil
			return
		}
		body := strings.TrimSpace(strings.Join(buf, "\n"))
		buf = nil
		if body == "" { // heading-only section (e.g. a bare container heading)
			cur = nil
			return
		}
		body, _ = capBytes(body, maxChunkSourceBytes)
		cur.Body = body
		if m := componentClassRe.FindStringSubmatch(body); m != nil {
			cur.componentClass = m[1]
		}
		cur.headingTok = tokenize(cur.Heading)
		cur.bodyTok = tokenize(body)
		chunks = append(chunks, *cur)
		cur = nil
	}

	for _, line := range lines {
		if isFence(line) {
			inFence = !inFence
			if cur != nil {
				buf = append(buf, line)
			}
			continue
		}
		if !inFence {
			if m := headingRe.FindStringSubmatch(line); len(m) > 1 && len(m[1]) <= componentLevel {
				level := len(m[1])
				text := m[2]
				flush()
				for len(stack) > 0 && stack[len(stack)-1].level >= level {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, stackEntry{level: level, text: text})
				crumb := make([]string, 0, len(stack)+1)
				crumb = append(crumb, src.Name)
				for _, e := range stack {
					crumb = append(crumb, e.text)
				}
				cur = &Chunk{
					Source:     src.Name,
					SourceID:   src.ID,
					Breadcrumb: strings.Join(crumb, " > "),
					Heading:    text,
					Level:      level,
				}
				continue
			}
		}
		if cur != nil {
			buf = append(buf, line)
		}
	}
	flush()
	return chunks
}

// stripFrontmatter drops a leading "---"-fenced frontmatter block (the daisyUI
// llms.txt opens with one) so its metadata doesn't leak into a chunk body.
func stripFrontmatter(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) == 0 || strings.TrimRight(lines[0], "\r") != "---" {
		return s
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r") == "---" {
			return strings.Join(lines[i+1:], "\n")
		}
	}
	return s
}

// isFence reports whether a line opens or closes a markdown code fence.
func isFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}
