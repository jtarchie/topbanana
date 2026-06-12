// Package lint validates generated HTML — parse errors, broken relative
// links, broken anchor fragments, and per-template invariants. Failures
// piggyback on the build retry loop so the agent gets concrete fix
// instructions.
package lint

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"strings"

	"golang.org/x/net/html"

	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// Kind classifies an Error so the build loop can decide whether to attempt
// a deterministic auto-fix or hand the error to the agent. An empty Kind
// is the default and always means "agent must fix" — safe fallback for
// anything we have not categorized yet.
type Kind string

const (
	// KindDesignSubstrate identifies a page missing the self-hosted /app.css
	// stylesheet link — purely mechanical, AutoFixDesignSubstrate handles it.
	KindDesignSubstrate Kind = "design_substrate"
	// KindSuspiciousAttr identifies an unclosed quoted attribute that
	// swallowed a following element. Auto-fix is unsafe here because
	// blindly injecting more tags on top of a parser-recovery bug would
	// deepen the corruption — only the agent can repair the original
	// quoting before any substrate fix is meaningful.
	KindSuspiciousAttr Kind = "suspicious_attr"
	// KindMobileViewport identifies a page missing a responsive viewport
	// declaration (`<meta name="viewport" content="width=device-width, ...">`).
	// Without it mobile browsers render the page at a ~980px desktop width and
	// zoom out, so the design is not mobile-friendly. Mechanical when the tag
	// is simply absent — AutoFixMobileViewport injects the canonical form.
	KindMobileViewport Kind = "mobile_viewport"
	// KindBrokenAnchor identifies an href fragment (`#id`) that doesn't match
	// any element id (or `<a name>`) on the page the link resolves to. Never
	// auto-fixed: only the agent can decide whether the right repair is giving
	// the intended element that id or correcting the href.
	KindBrokenAnchor Kind = "broken_anchor"
	// KindMissingCharset identifies a page with no character-encoding
	// declaration, so browsers guess and non-ASCII text can render as
	// mojibake. Purely mechanical — AutoFixCharset injects the canonical
	// <meta charset="utf-8"> at the top of <head>.
	KindMissingCharset Kind = "missing_charset"
	// KindMissingLang identifies an <html> element without a lang attribute.
	// Not auto-fixed: only the agent knows what language the site is actually
	// written in, and a wrong default is worse than none.
	KindMissingLang Kind = "missing_lang"
	// KindMissingTitle identifies a page with no non-empty <title>. Not
	// auto-fixed: the title needs real content.
	KindMissingTitle Kind = "missing_title"
	// KindDuplicateTitle identifies a page whose <title> text is identical to
	// another page's, making tabs, history, and search results
	// indistinguishable. The agent decides how to differentiate them.
	KindDuplicateTitle Kind = "duplicate_title"
	// KindMissingDescription identifies a page with no non-empty
	// <meta name="description">, so search results and link previews fall
	// back to arbitrary page text. Not auto-fixed: the summary needs real
	// content only the agent can write.
	KindMissingDescription Kind = "missing_description"
	// KindFormControlUnnamed identifies an input/select/textarea inside a
	// submitting form with no name attribute — the browser silently drops
	// its value from the submission.
	KindFormControlUnnamed Kind = "form_control_unnamed"
	// KindFormPostNoAction identifies a <form method="post"> with no action:
	// the post goes back to the static page itself and the data is discarded.
	KindFormPostNoAction Kind = "form_post_no_action"
	// KindMultipartForm identifies a file input or multipart enctype — the
	// platform's /api/ functions parse only URL-encoded and JSON bodies, so
	// multipart submissions arrive unreadable.
	KindMultipartForm Kind = "multipart_unsupported"
	// KindBrokenFetch identifies an inline-script fetch() whose literal URL
	// resolves to nothing — a missing /api/ function or a missing file.
	KindBrokenFetch Kind = "broken_fetch"
)

type Error struct {
	File    string
	Message string
	Kind    Kind
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

	lc := linkCheckContext{fileSet: fileSet, enablesFns: tmpl != nil && tmpl.EnablesFunctions}

	var errs []Error
	var pages []pageInfo
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
			pi := collectPageInfo(file, doc)
			pages = append(pages, pi)
			facts := collectJSFacts(file, pi.scripts)
			errs = append(errs, checkHTMLLinks(file, doc, lc)...)
			errs = append(errs, checkInlineJS(file, pi.scripts)...)
			errs = append(errs, suspiciousAttrValues(file, doc)...)
			errs = append(errs, checkDesignSubstrate(file, doc)...)
			errs = append(errs, checkMobileViewport(file, doc)...)
			errs = append(errs, checkHeadHygiene(pi)...)
			errs = append(errs, checkForms(pi)...)
			errs = append(errs, checkFetchTargets(pi, facts, lc)...)
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

	// Cross-page checks (a fragment can target an id on another page; titles
	// must be unique across the site) run once every page is parsed rather
	// than per file above.
	errs = append(errs, checkAnchors(pages, lc)...)
	errs = append(errs, checkDuplicateTitles(pages)...)

	errs = append(errs, checkTemplateInvariants(ctx, s, slug, tmpl)...)
	errs = append(errs, checkEntryPoint(ctx, s, slug)...)

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

// checkEntryPoint enforces the one invariant every site shares regardless of
// template: a non-empty index.html. Without it, a build where the agent wrote
// no HTML at all — e.g. a weaker model that answered in prose instead of
// calling write_file — would lint clean (there are no HTML files to find fault
// with) and report success while serving nothing. store.Read returns an empty
// object with no error for a missing key, so the empty-content test also
// covers "index.html absent". Surfaced into the build retry loop so the agent
// gets a concrete instruction; if it still produces nothing, the build fails.
func checkEntryPoint(ctx context.Context, s *store.Store, slug string) []Error {
	obj, err := s.Read(ctx, slug, "index.html")
	if err != nil {
		return []Error{{File: "index.html", Message: fmt.Sprintf("could not read index.html: %s", err)}}
	}
	if strings.TrimSpace(obj.Content) == "" {
		return []Error{{File: "index.html", Message: "site is missing a non-empty index.html (every site needs an entry point)"}}
	}
	return nil
}

// The DaisyUI + Tailwind JIT pair is the design substrate every generated
// page must load — without them DaisyUI components render as bare elements
// and pages drift back to the dated default the system prompt is trying to
// kill. Match the host (not the full URL) so version bumps in the agent's
// output don't make the lint flake.
//
// localStylesheetHref is the self-hosted stylesheet every page must link. It
// is compiled per site by the post-build CSS step (build.optimizeCSS) and
// served at /app.css — it bundles DaisyUI, every theme, and the Tailwind
// utilities the page uses. There is no CDN substrate anymore.
const localStylesheetHref = "/app.css"

// localStylesheetTag is the canonical form AutoFixDesignSubstrate injects.
const localStylesheetTag = `<link rel="stylesheet" href="/app.css">`

// WalkDOM does a depth-first pre-order traversal of the parse tree, invoking
// visit on every node. Every DOM-based check shares it instead of redeclaring
// the recursive closure inline. Exported so the sibling internal/guide package
// (the owner-facing completeness checks) can reuse the one traversal rather
// than duplicating it.
func WalkDOM(n *html.Node, visit func(*html.Node)) {
	visit(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		WalkDOM(c, visit)
	}
}

// injectBeforeHeadClose splices tag (plus a newline) in immediately before the
// first case-insensitive </head>. Returns (content, false) unchanged when the
// document has no </head>. The auto-fixers share it so the "insert into <head>"
// mechanics live in one place.
func injectBeforeHeadClose(content, tag string) (string, bool) {
	closeIdx := strings.Index(strings.ToLower(content), "</head>")
	if closeIdx == -1 {
		return content, false
	}
	return content[:closeIdx] + tag + "\n" + content[closeIdx:], true
}

// injectAfterHeadOpen splices tag (plus a newline) in immediately after the
// first case-insensitive <head ...> opening tag. The charset auto-fixer needs
// this spot: the HTML spec requires the encoding declaration within the first
// 1024 bytes, and injecting before </head> could land it after a long inline
// <style> that already blew that budget. The boundary check on the byte after
// "<head" keeps <header> from matching. Returns (content, false) when no
// <head> open tag exists.
func injectAfterHeadOpen(content, tag string) (string, bool) {
	lower := strings.ToLower(content)
	from := 0
	for {
		i := strings.Index(lower[from:], "<head")
		if i == -1 {
			return content, false
		}
		i += from
		after := i + len("<head")
		if after >= len(lower) {
			return content, false
		}
		switch lower[after] {
		case '>', ' ', '\t', '\n', '\r', '/':
			end := strings.IndexByte(content[i:], '>')
			if end == -1 {
				return content, false
			}
			insert := i + end + 1
			return content[:insert] + "\n" + tag + content[insert:], true
		default:
			from = after
		}
	}
}

// substratePresence records whether the self-hosted stylesheet link was found
// as a well-formed DOM node during one pass over a document.
type substratePresence struct {
	local bool
}

func (p *substratePresence) inspect(n *html.Node) {
	if n.Type != html.ElementNode || n.Data != "link" {
		return
	}
	for _, a := range n.Attr {
		if a.Key == "href" && a.Val == localStylesheetHref {
			p.local = true
		}
	}
}

// checkDesignSubstrate verifies a page links the self-hosted /app.css
// stylesheet as a well-formed DOM element — not just bytes that appear
// somewhere in the file. Substring matching would miss the failure where a
// previous attribute (typically a <meta> viewport whose content="" lost its
// closing quote) swallows the <link> during parser recovery: the href still
// appears in the file but no <link> exists in the DOM and the page renders
// unstyled.
func checkDesignSubstrate(file string, doc *html.Node) []Error {
	var p substratePresence
	WalkDOM(doc, p.inspect)

	if p.local {
		return nil
	}
	return []Error{{
		File:    file,
		Kind:    KindDesignSubstrate,
		Message: "missing stylesheet — every page must include `<link rel=\"stylesheet\" href=\"/app.css\">` in <head> as a well-formed element. /app.css is the self-hosted DaisyUI + Tailwind sheet (no CDN). If the href appears in the file but this check still fires, an earlier attribute (often a <meta> viewport) is missing a closing quote and is consuming the <link> tag.",
	}}
}

// AutoFixDesignSubstrate injects the self-hosted stylesheet link right before
// the closing </head> when it's missing. Idempotent: a page that already
// links /app.css (per the same DOM walk checkDesignSubstrate uses) is left
// untouched. Returns the new content and whether anything changed.
//
// Callers must NOT invoke this on a file that also produced a
// KindSuspiciousAttr error — a parser-recovery bug there means the href may
// appear in the bytes but be swallowed by a broken attribute, and adding more
// tags would compound the corruption rather than repair it. The build loop
// checks for that before calling in.
func AutoFixDesignSubstrate(content string) (string, bool) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return content, false
	}
	var p substratePresence
	WalkDOM(doc, p.inspect)
	if p.local {
		return content, false
	}
	return injectBeforeHeadClose(content, localStylesheetTag)
}

// viewportMetaTag is the canonical responsive viewport declaration every page
// must carry so phones render at their real width instead of a zoomed-out
// ~980px desktop viewport. AutoFixMobileViewport injects exactly this form, and
// the CSS step (build.swapSubstrateForLocalCSS) injects an identical tag.
const viewportMetaTag = `<meta name="viewport" content="width=device-width, initial-scale=1">`

// viewportPresence records, during one document walk, whether a viewport meta
// exists at all (meta) and whether it is responsive — its content opts into
// the device width (responsive). The split lets the lint flag a present-but-
// non-responsive tag while AutoFixMobileViewport declines to stack a second
// meta on top of one that already exists (the author must repair the value).
type viewportPresence struct {
	meta       bool
	responsive bool
}

func (p *viewportPresence) inspect(n *html.Node) {
	if n.Type != html.ElementNode || n.Data != "meta" {
		return
	}
	var isViewport bool
	var content string
	for _, a := range n.Attr {
		switch {
		case strings.EqualFold(a.Key, "name") && strings.EqualFold(strings.TrimSpace(a.Val), "viewport"):
			isViewport = true
		case strings.EqualFold(a.Key, "content"):
			content = a.Val
		}
	}
	if !isViewport {
		return
	}
	p.meta = true
	if strings.Contains(strings.ToLower(content), "width=device-width") {
		p.responsive = true
	}
}

// checkMobileViewport verifies a page declares a responsive viewport — a
// `<meta name="viewport">` element whose content opts into width=device-width.
// A missing tag, or one pinned to a desktop width, both fail: mobile browsers
// would render the page at a ~980px viewport and zoom out. Element-based (like
// checkDesignSubstrate) so a tag swallowed by a malformed earlier attribute is
// not mistaken for present.
func checkMobileViewport(file string, doc *html.Node) []Error {
	var p viewportPresence
	WalkDOM(doc, p.inspect)

	if p.responsive {
		return nil
	}
	return []Error{{
		File:    file,
		Kind:    KindMobileViewport,
		Message: "missing responsive viewport — every page must include `<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">` in <head>. Without width=device-width, phones render the page at a ~980px desktop width and zoom out, so the design is not mobile-friendly.",
	}}
}

// AutoFixMobileViewport injects the canonical viewport meta right before the
// closing </head> when the page carries no viewport meta at all. Idempotent,
// and deliberately conservative: a page that already has a viewport meta — even
// a non-responsive one — is left untouched so the fix never stacks a second,
// conflicting meta on top of one the author wrote (checkMobileViewport still
// flags the bad value for the agent to repair). Like AutoFixDesignSubstrate,
// callers must not invoke it on a file that also produced a KindSuspiciousAttr
// error. Returns the new content and whether anything changed.
func AutoFixMobileViewport(content string) (string, bool) {
	doc, err := html.Parse(strings.NewReader(content))
	if err != nil {
		return content, false
	}
	var p viewportPresence
	WalkDOM(doc, p.inspect)
	if p.meta {
		return content, false
	}
	return injectBeforeHeadClose(content, viewportMetaTag)
}

// AutoFixers maps each deterministically fixable lint Kind to the in-code
// transform that repairs it. The build retry loop (build.autoFixLint) applies
// every applicable fixer before falling back to an agent turn, and the MCP
// lint_site result marks a problem autofixable iff its Kind is a key here.
// Each fixer is idempotent and injects into <head> (the substrate and
// viewport before </head>, the charset right after <head> where the spec's
// 1024-byte budget is safe), so several may be chained over one page in any
// order.
var AutoFixers = map[Kind]func(string) (string, bool){
	KindDesignSubstrate: AutoFixDesignSubstrate,
	KindMobileViewport:  AutoFixMobileViewport,
	KindMissingCharset:  AutoFixCharset,
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
	WalkDOM(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		for _, attr := range n.Attr {
			name := findEmbeddedTagName(attr.Val)
			if name == "" {
				continue
			}
			errs = append(errs, Error{
				File:    file,
				Kind:    KindSuspiciousAttr,
				Message: fmt.Sprintf("<%s> attribute %q has a value containing an embedded <%s> tag — the value is missing a closing quote and is swallowing the following element. Re-read the file, then rewrite the broken attribute so its quoted value ends before the next tag begins.", n.Data, attr.Key, name),
			})
			// One error per element is plenty; the agent will re-read
			// the whole file to fix it.
			break
		}
	})
	return errs
}

// findEmbeddedTagName returns the first known HTML tag name found after a
// "<" inside v, or "" when none. Match is case-insensitive on the tag name.
func findEmbeddedTagName(v string) string {
	for i := range len(v) {
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

// linkCheckContext bundles the per-site state the link checks thread through:
// the set of known files (for resolving relative links) and whether the site
// has server-side functions enabled (so /api/* routes aren't flagged as
// broken). Passing it as one value keeps the three-function chain from
// threading the same two arguments each.
type linkCheckContext struct {
	fileSet    map[string]bool
	enablesFns bool
}

// checkHTMLLinks walks a parsed HTML tree and checks all relative href/src
// attributes against the known file set. External URLs are skipped. When
// lc.enablesFns is true, absolute /api/* paths are treated as valid dynamic
// routes (handled by apiHandler at runtime) and not flagged as broken.
func checkHTMLLinks(filename string, doc *html.Node, lc linkCheckContext) []Error {
	dir := path.Dir(filename)
	var errs []Error

	WalkDOM(doc, func(n *html.Node) {
		if n.Type == html.ElementNode {
			errs = append(errs, checkNodeLinks(filename, dir, n, lc)...)
		}
	})

	return errs
}

func checkNodeLinks(filename, dir string, n *html.Node, lc linkCheckContext) []Error {
	var errs []Error
	for _, attr := range n.Attr {
		if attr.Key != "href" && attr.Key != "src" && attr.Key != "action" {
			continue
		}
		errs = append(errs, checkLink(filename, dir, attr.Val, lc)...)
	}
	return errs
}

// checkLink returns the broken-link error for one href/src value as a slice
// (empty when the link is fine), matching the []Error contract the other
// checks use. All normalization and exemption logic lives in
// resolveSiteTarget (links.go).
func checkLink(filename, dir, rawVal string, lc linkCheckContext) []Error {
	resolved, ok, skip := resolveSiteTarget(dir, rawVal, lc)
	if skip || ok {
		return nil
	}
	return []Error{{
		File:    filename,
		Message: brokenLinkMessage(rawVal, resolved, lc.fileSet),
	}}
}

func IsExternalLink(link string) bool {
	lower := strings.ToLower(link)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "mailto:") ||
		strings.HasPrefix(lower, "tel:") ||
		strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "data:")
}
