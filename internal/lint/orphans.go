package lint

import (
	"fmt"
)

// This file owns the unreferenced-page check: a .html file that exists but
// that no other page links to, no inline script navigates to, and no
// functions/*.js redirect mentions. The owner believes the page is live;
// visitors can never find it — typically a leftover from a rename or a page
// the agent wrote and forgot to put in the nav.

// checkUnreferencedPages flags non-index pages with zero references.
// "Referenced" means: a resolved href/src/action anywhere (self-references
// excluded — a page whose only mention is its own navbar is still
// unreachable), a string literal in any page's inline scripts that resolves
// to it (location.href = 'thanks.html'), or a string literal in any
// functions/*.js (response.redirect("/thanks.html") — resolved from the site
// root, since redirects are sent as absolute paths). The extensionless
// fallback applies throughout, which is deliberately lenient: a stray
// literal can mark a page referenced (false-negative direction), but a
// false "unreachable" would block a build.
//
// skeletonPages are exempt: pages the chosen template ships deliberately
// unlinked (e.g. tiny-shop's owner-facing /orders.html order log) are the
// template author's design, not an agent mistake.
func checkUnreferencedPages(pages []pageInfo, factsByPage map[string]jsFacts, fnLiterals []string, skeletonPages map[string]bool, lc linkCheckContext) []Error {
	referenced := collectReferences(pages, factsByPage, fnLiterals, lc)

	var errs []Error
	for _, p := range pages {
		if p.name == "index.html" || referenced[p.name] || skeletonPages[p.name] {
			continue
		}
		errs = append(errs, Error{
			File: p.name,
			Kind: KindUnreferencedPage,
			Message: fmt.Sprintf(
				`unreachable page — %s exists but nothing links to it: no href/src/action on any other page, no inline-script URL, and no functions/*.js redirect mentions it, so visitors can never find it. Add a link to it from the shared nav (or wherever it belongs), or delete the file if it's leftover.`,
				p.name),
		})
	}
	return errs
}

// collectReferences resolves every reference-shaped value in the site —
// href/src/action attributes, inline-script string literals, and
// functions/*.js string literals (root-relative, since redirects are sent as
// absolute paths) — into the set of files they reach. Self-references don't
// count.
func collectReferences(pages []pageInfo, factsByPage map[string]jsFacts, fnLiterals []string, lc linkCheckContext) map[string]bool {
	referenced := map[string]bool{}
	mark := func(dir, val, self string) {
		resolved, ok, _ := resolveSiteTarget(dir, val, lc)
		if ok && resolved != self {
			referenced[resolved] = true
		}
	}

	for _, p := range pages {
		for _, n := range p.elements {
			for _, a := range n.Attr {
				if a.Namespace != "" {
					continue
				}
				if a.Key == "href" || a.Key == "src" || a.Key == "action" {
					mark(p.dir, a.Val, p.name)
				}
			}
		}
		for _, lit := range factsByPage[p.name].stringLiterals {
			mark(p.dir, lit, p.name)
		}
	}
	for _, lit := range fnLiterals {
		mark(".", lit, "")
	}
	return referenced
}
