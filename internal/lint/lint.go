// Package lint validates generated HTML — parse errors, broken relative
// links, and per-template invariants. Failures piggyback on the build retry
// loop so the agent gets concrete fix instructions.
package lint

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"golang.org/x/net/html"

	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
)

type Error struct {
	File    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%s: %s", e.File, e.Message)
}

// App validates all HTML files in a slug: checks HTML parse errors, ensures
// all relative href/src links resolve to existing files in the store, and
// runs any per-template invariants. tmpl may be nil.
func App(ctx context.Context, s *store.Store, slug string, tmpl *templates.SiteTemplate) []Error {
	files, err := s.List(ctx, slug)
	if err != nil {
		return []Error{{File: slug, Message: fmt.Sprintf("failed to list files: %s", err)}}
	}

	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	var errs []Error
	for _, file := range files {
		switch {
		case strings.HasSuffix(file, ".html"):
			obj, err := s.Read(ctx, slug, file)
			if err != nil || obj.Content == "" {
				errs = append(errs, Error{File: file, Message: "could not read file"})
				continue
			}
			doc, parseErr := html.Parse(strings.NewReader(obj.Content))
			if parseErr != nil {
				errs = append(errs, Error{File: file, Message: fmt.Sprintf("HTML parse error: %s", parseErr)})
				continue
			}
			errs = append(errs, checkHTMLLinks(file, doc, fileSet, tmpl != nil && tmpl.EnablesFunctions)...)
			errs = append(errs, checkInlineJS(file, doc)...)
			errs = append(errs, suspiciousAttrValues(file, doc)...)
			errs = append(errs, checkDesignSubstrate(file, doc)...)
		case strings.HasSuffix(file, ".js"):
			// JS files are allowed under functions/ only — JSFile rejects
			// .js files anywhere else. The agent's path validation also
			// blocks this, but we double-check here so a hand-edited site
			// can't smuggle JS into HTML paths.
			obj, err := s.Read(ctx, slug, file)
			if err != nil || obj.Content == "" {
				errs = append(errs, Error{File: file, Message: "could not read file"})
				continue
			}
			errs = append(errs, JSFile(file, obj.Content)...)
		}
	}

	errs = append(errs, checkTemplateInvariants(ctx, s, slug, tmpl)...)

	if len(errs) > 0 {
		slog.Warn("lint.app.errors", "slug", slug, "count", len(errs))
		for _, e := range errs {
			slog.Warn("lint.error", "slug", slug, "file", e.File, "message", e.Message)
		}
	} else {
		slog.Info("lint.app.ok", "slug", slug)
	}

	return errs
}

// The DaisyUI + Tailwind JIT pair is the design substrate every generated
// page must load — without them DaisyUI components render as bare elements
// and pages drift back to the dated default the system prompt is trying to
// kill. Match the host (not the full URL) so version bumps in the agent's
// output don't make the lint flake.
const (
	daisyHost    = "cdn.jsdelivr.net/npm/daisyui"
	tailwindHost = "cdn.jsdelivr.net/npm/@tailwindcss/browser"
)

// checkDesignSubstrate verifies a page links both halves of the design
// substrate as well-formed DOM elements — not just bytes that appear
// somewhere in the file. Substring matching missed the failure where a
// previous attribute (typically a <meta> viewport whose content="" lost
// its closing quote) swallows the next tag during parser recovery: the
// substrate URL still appears in the file but no <link>/<script> exists
// in the DOM and the page renders without DaisyUI.
func checkDesignSubstrate(file string, doc *html.Node) []Error {
	var hasDaisy, hasTailwind bool
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "link":
				for _, a := range n.Attr {
					if a.Key == "href" && strings.Contains(a.Val, daisyHost) {
						hasDaisy = true
					}
				}
			case "script":
				for _, a := range n.Attr {
					if a.Key == "src" && strings.Contains(a.Val, tailwindHost) {
						hasTailwind = true
					}
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	var errs []Error
	if !hasDaisy {
		errs = append(errs, Error{
			File:    file,
			Message: "missing DaisyUI stylesheet — every page must include `<link href=\"https://cdn.jsdelivr.net/npm/daisyui@5\" rel=\"stylesheet\" type=\"text/css\" />` in <head> as a well-formed element. If the URL appears in the file but this check still fires, an earlier attribute (often a <meta> viewport) is missing a closing quote and is consuming the <link> tag.",
		})
	}
	if !hasTailwind {
		errs = append(errs, Error{
			File:    file,
			Message: "missing Tailwind browser script — every page must include `<script src=\"https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4\"></script>` in <head> as a well-formed element.",
		})
	}
	return errs
}

// suspiciousAttrValues flags an attribute value that contains an embedded
// "<tagname" sequence where tagname matches a known HTML element. This is
// the smoking-gun signature of an unclosed quoted attribute that swallowed
// a following sibling tag during HTML5 parser recovery — golang.org/x/net/html
// (and every browser) silently absorbs the swallowed tag's bytes into the
// preceding attribute, so html.Parse returns no error, the file "renders,"
// and the swallowed element is just gone from the DOM. The model's resume
// build hit this with a viewport <meta content="..."> consuming the next
// <link href="...daisyui..."> on the same line.
func suspiciousAttrValues(file string, doc *html.Node) []Error {
	var errs []Error
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			for _, attr := range n.Attr {
				name := findEmbeddedTagName(attr.Val)
				if name == "" {
					continue
				}
				errs = append(errs, Error{
					File:    file,
					Message: fmt.Sprintf("<%s> attribute %q has a value containing an embedded <%s> tag — the value is missing a closing quote and is swallowing the following element. Re-read the file, then rewrite the broken attribute so its quoted value ends before the next tag begins.", n.Data, attr.Key, name),
				})
				// One error per element is plenty; the agent will re-read
				// the whole file to fix it.
				break
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return errs
}

// findEmbeddedTagName returns the first known HTML tag name found after a
// "<" inside v, or "" when none. Match is case-insensitive on the tag name.
func findEmbeddedTagName(v string) string {
	for i := 0; i < len(v); i++ {
		if v[i] != '<' {
			continue
		}
		end := i + 1
		for end < len(v) && isASCIILetter(v[end]) {
			end++
		}
		if end == i+1 {
			continue
		}
		name := strings.ToLower(v[i+1 : end])
		if knownHTMLTags[name] {
			return name
		}
	}
	return ""
}

func isASCIILetter(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// knownHTMLTags is the allowlist suspiciousAttrValues checks against. Keep
// it to elements the agent actually emits — too broad triggers false
// positives on legitimate "<" characters in onclick handlers and JS-ish
// attribute values; too narrow misses real bugs.
var knownHTMLTags = map[string]bool{
	"a": true, "article": true, "aside": true, "body": true, "br": true,
	"button": true, "div": true, "footer": true, "form": true, "h1": true,
	"h2": true, "h3": true, "h4": true, "h5": true, "h6": true, "head": true,
	"header": true, "hr": true, "html": true, "iframe": true, "img": true,
	"input": true, "label": true, "li": true, "link": true, "main": true,
	"menu": true, "meta": true, "nav": true, "ol": true, "option": true,
	"p": true, "script": true, "section": true, "select": true, "span": true,
	"strong": true, "style": true, "svg": true, "table": true, "tbody": true,
	"td": true, "textarea": true, "th": true, "title": true, "tr": true,
	"ul": true,
}

// checkTemplateInvariants runs declarative must_contain checks for the chosen
// template.
func checkTemplateInvariants(ctx context.Context, s *store.Store, slug string, tmpl *templates.SiteTemplate) []Error {
	if tmpl == nil || len(tmpl.Checks) == 0 {
		return nil
	}

	var errs []Error
	for _, check := range tmpl.Checks {
		obj, err := s.Read(ctx, slug, check.File)
		if err != nil || obj.Content == "" {
			errs = append(errs, Error{
				File:    check.File,
				Message: fmt.Sprintf("required by %q template but missing or empty", tmpl.ID),
			})
			continue
		}
		for _, must := range check.MustContain {
			if strings.Contains(obj.Content, must) {
				continue
			}
			msg := check.Message
			if msg == "" {
				msg = fmt.Sprintf("must contain %q (template %q)", must, tmpl.ID)
			} else {
				msg = fmt.Sprintf("%s (missing %q)", msg, must)
			}
			errs = append(errs, Error{File: check.File, Message: msg})
		}
	}
	return errs
}

// checkHTMLLinks walks a parsed HTML tree and checks all relative href/src
// attributes against the known file set. External URLs are skipped. When
// enablesFns is true, absolute /api/* paths are treated as valid dynamic
// routes (handled by apiHandler at runtime) and not flagged as broken.
func checkHTMLLinks(filename string, doc *html.Node, fileSet map[string]bool, enablesFns bool) []Error {
	dir := path.Dir(filename)
	var errs []Error

	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			errs = append(errs, checkNodeLinks(filename, dir, n, fileSet, enablesFns)...)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	return errs
}

func checkNodeLinks(filename, dir string, n *html.Node, fileSet map[string]bool, enablesFns bool) []Error {
	var errs []Error
	for _, attr := range n.Attr {
		if attr.Key != "href" && attr.Key != "src" && attr.Key != "action" {
			continue
		}
		err := checkLink(filename, dir, attr.Val, fileSet, enablesFns)
		if err != nil {
			errs = append(errs, *err)
		}
	}
	return errs
}

func checkLink(filename, dir, rawVal string, fileSet map[string]bool, enablesFns bool) *Error {
	link := strings.TrimSpace(rawVal)
	if link == "" || link == "#" || isExternalLink(link) {
		return nil
	}
	if i := strings.IndexByte(link, '#'); i != -1 {
		link = link[:i]
	}
	if i := strings.IndexByte(link, '?'); i != -1 {
		link = link[:i]
	}
	if link == "" {
		return nil
	}
	// Dynamic API routes are served by apiHandler, not by static files. When
	// the template enables functions, treat /api/* as always valid — the lint
	// has no way to know which {name} handlers the agent has authored, and
	// functions/{name}.js may not yet be written when the page is linted.
	if enablesFns && strings.HasPrefix(link, "/api/") {
		return nil
	}
	resolved := path.Join(dir, link)
	if fileSet[resolved] || fileSet[resolved+".html"] || fileSet[path.Join(resolved, "index.html")] {
		return nil
	}
	return &Error{
		File:    filename,
		Message: fmt.Sprintf("broken link %q (resolved to %q)", rawVal, resolved),
	}
}

func isExternalLink(link string) bool {
	lower := strings.ToLower(link)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "data:")
}
