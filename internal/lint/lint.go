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
			errs = append(errs, checkDesignSubstrate(file, obj.Content)...)
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
// substrate. Returns one error per missing piece so the agent gets a
// specific fix prompt rather than a generic "substrate missing".
func checkDesignSubstrate(file, content string) []Error {
	var errs []Error
	if !strings.Contains(content, daisyHost) {
		errs = append(errs, Error{
			File:    file,
			Message: "missing DaisyUI stylesheet — every page must include `<link href=\"https://cdn.jsdelivr.net/npm/daisyui@5\" rel=\"stylesheet\" type=\"text/css\" />` in <head>",
		})
	}
	if !strings.Contains(content, tailwindHost) {
		errs = append(errs, Error{
			File:    file,
			Message: "missing Tailwind browser script — every page must include `<script src=\"https://cdn.jsdelivr.net/npm/@tailwindcss/browser@4\"></script>` in <head>",
		})
	}
	return errs
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
