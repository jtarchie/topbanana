package lint

import (
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html"
)

// This file owns the self-containment checks. The platform's promise is that
// a site keeps working with zero external dependencies: a third-party script
// or stylesheet can vanish, change, or be blocked, and the owner would never
// connect "my site looks broken" to a CDN dying. Plain http:// resources are
// worse — the site is served over https, so browsers block or warn on them
// today. https iframes and images are deliberately NOT flagged: map embeds,
// videos, and hotlinked images are legitimate content choices.

// allowedScriptHosts are the platform-blessed third-party script origins:
// Stripe payment embeds, which the tiny-shop and pricing templates
// intentionally ship and which cannot be inlined.
var allowedScriptHosts = map[string]bool{
	"js.stripe.com":  true,
	"buy.stripe.com": true,
}

// checkExternalResources flags external <script src>, external stylesheet
// <link>s, and any plain-http resource URL. Only un-namespaced attributes
// are inspected, so SVG xlink:href and xmlns declarations can never match.
func checkExternalResources(pi pageInfo) []Error {
	var errs []Error
	seen := map[string]bool{}
	for _, n := range pi.elements {
		switch n.Data {
		case "script":
			e := checkScriptSrc(pi.name, n)
			if e != nil {
				errs = append(errs, *e)
				continue // one error per element; no http:// double-report
			}
		case "link":
			e := checkStylesheetHref(pi.name, n)
			if e != nil {
				errs = append(errs, *e)
				continue
			}
		}
		errs = append(errs, checkInsecureURLs(pi.name, n, seen)...)
	}
	return errs
}

// isExternalResource reports whether a src/href points off-site: absolute
// http(s) or protocol-relative.
func isExternalResource(v string) bool {
	lower := strings.ToLower(v)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "//")
}

func checkScriptSrc(filename string, n *html.Node) *Error {
	src := strings.TrimSpace(attrVal(n, "src"))
	if src == "" || !isExternalResource(src) {
		return nil
	}
	u, err := url.Parse(src)
	if err == nil && allowedScriptHosts[strings.ToLower(u.Hostname())] {
		// Allowlisted host — not an external-script violation. An http://
		// Stripe URL is still wrong, but that's the mixed-content check's
		// finding (and its repair, https://, is the right one).
		return nil
	}
	return &Error{
		File: filename,
		Kind: KindExternalScript,
		Message: fmt.Sprintf(
			`external script — <script src=%q> loads third-party JavaScript, but sites here are fully self-contained: if that host changes or goes down, the page silently breaks. Inline the logic in a <script> on the page instead. (Stripe embeds are the one exception: https://js.stripe.com and https://buy.stripe.com are allowed.)`,
			src),
	}
}

func checkStylesheetHref(filename string, n *html.Node) *Error {
	if !linkRelContains(n, "stylesheet") {
		return nil
	}
	href := strings.TrimSpace(attrVal(n, "href"))
	if href == "" || !isExternalResource(href) {
		return nil
	}
	return &Error{
		File: filename,
		Kind: KindExternalStylesheet,
		Message: fmt.Sprintf(
			`external stylesheet — <link rel="stylesheet" href=%q> loads CSS from another origin, but sites here are fully self-contained: the only stylesheet is the self-hosted /app.css (DaisyUI components, every theme, and the Tailwind utilities). Rebuild the styles with DaisyUI/Tailwind classes or an inline <style>, and remove this link.`,
			href),
	}
}

func linkRelContains(n *html.Node, token string) bool {
	for _, t := range strings.Fields(attrVal(n, "rel")) {
		if strings.EqualFold(t, token) {
			return true
		}
	}
	return false
}

// checkInsecureURLs flags plain-http URLs in src/href/action. The site is
// served over https, so browsers block http:// subresources outright and
// warn on http:// navigation — either way the owner's content doesn't reach
// the visitor. Each distinct URL is reported once per page.
func checkInsecureURLs(filename string, n *html.Node, seen map[string]bool) []Error {
	var errs []Error
	for _, a := range n.Attr {
		if a.Namespace != "" {
			continue
		}
		if a.Key != "src" && a.Key != "href" && a.Key != "action" {
			continue
		}
		val := strings.TrimSpace(a.Val)
		if !strings.HasPrefix(strings.ToLower(val), "http://") || seen[val] {
			continue
		}
		seen[val] = true
		errs = append(errs, Error{
			File: filename,
			Kind: KindMixedContent,
			Message: fmt.Sprintf(
				`insecure http:// URL — %s=%q on <%s> uses plain http. The site is served over https, so browsers block or warn on this content and it may never load for visitors. Switch the URL to https://, or copy the asset into the site and use a relative path.`,
				a.Key, val, n.Data),
		})
	}
	return errs
}
