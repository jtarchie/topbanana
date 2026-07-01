package lint

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// This file owns link-target resolution and the agent-facing wording for
// broken links. The resolver is the single source of truth shared by the
// page-link check (checkLink) and the anchor check (checkAnchorHref), so the
// two can never disagree about which file a link points at.

// resolveSiteTarget normalizes one href/src/action (or fetch URL) value the
// way checkLink always has — trim, drop #fragment and ?query, exempt
// external schemes, /api/ routes (when functions are enabled or a backing
// functions/{name}.js exists), and the post-build /app.css — then resolves
// what's left against the file set. skip=true means the value is not
// validatable as a site file (empty, external, exempt); otherwise ok reports
// whether the proxy would serve it and resolved names the file (or the
// cleaned miss, for error messages). Shared by the page-link, fetch, and
// page-reference checks so they can never disagree.
//
// Note: <base href> is not honored — neither here nor in the serving proxy.
func resolveSiteTarget(dir, rawVal string, lc linkCheckContext) (resolved string, ok, skip bool) {
	link := strings.TrimSpace(rawVal)
	if link == "" || link == "#" || IsExternalLink(link) {
		return "", false, true
	}
	if i := strings.IndexByte(link, '#'); i != -1 {
		link = link[:i]
	}
	if i := strings.IndexByte(link, '?'); i != -1 {
		link = link[:i]
	}
	if link == "" {
		return "", false, true
	}
	// Dynamic API routes are served by apiHandler (internal/server/api.go),
	// not by static files: /api/{name} is backed by functions/{name}.js.
	// Treat such a link as valid when that backing file exists in the site,
	// or when functions are enabled (the {name} handler may not be authored
	// yet at lint time). The file-presence check keeps template-less sites —
	// which report enablesFns=false — from false-positiving real
	// function-backed forms. A dead /api/ route falls through and misses
	// resolution like any other path.
	if strings.HasPrefix(link, "/api/") {
		name := strings.TrimPrefix(link, "/api/")
		if lc.enablesFns || lc.fileSet["functions/"+name+".js"] {
			return "", false, true
		}
	}
	// /app.css is the self-hosted design substrate — compiled per site by the
	// post-build CSS step (so it isn't in the bucket when the page is linted)
	// and served by the platform. Always valid, never a broken link.
	if link == localStylesheetHref {
		return "", false, true
	}
	// The event-photo-wall endpoints (POST /_photos upload, GET /_photos/approved
	// poll) are served by the Go dispatch path, not static files. Treat them as
	// valid targets for the upload form's action and the display's fetch when the
	// template enables the wall — like /api/ routes when functions are enabled.
	if lc.photoWall && (link == photoUploadPath || link == photoApprovedPath || link == photoQRPath) {
		return "", false, true
	}
	resolved, found := resolveLinkTarget(dir, link, lc.fileSet)
	return resolved, found, false
}

// resolveLinkTarget resolves one link the way the serving proxy
// (internal/server/proxy.go) resolves request paths, so lint and production
// can never disagree about whether a link works:
//
//   - a leading "/" is site-root-absolute — the browser sends that path
//     unchanged, so it must NOT be joined onto the linking page's directory;
//   - relative links resolve against the page's directory, and ".." segments
//     that climb above the site root are dropped (browsers normalize them
//     away before the request is ever sent);
//   - the proxy's extensionless fallbacks ("x" → "x.html", "x/index.html")
//     apply only when the path does not already end in ".html".
//
// Returns the file the proxy would serve and whether it exists in fileSet;
// when it doesn't, the returned path is the cleaned form for error messages.
func resolveLinkTarget(dir, link string, fileSet map[string]bool) (string, bool) {
	var resolved string
	if strings.HasPrefix(link, "/") {
		resolved = path.Clean(strings.TrimPrefix(link, "/"))
	} else {
		resolved = path.Join(dir, link)
	}
	for strings.HasPrefix(resolved, "../") {
		resolved = strings.TrimPrefix(resolved, "../")
	}
	if resolved == "" || resolved == "." || resolved == ".." {
		resolved = "index.html"
	}

	candidates := []string{resolved}
	if !strings.HasSuffix(resolved, ".html") {
		candidates = append(candidates, resolved+".html", path.Join(resolved, "index.html"))
	}
	for _, c := range candidates {
		if fileSet[c] {
			return c, true
		}
	}
	return resolved, false
}

// brokenLinkMessage words a broken-link error for the agent: what was
// written, what it resolved to, the closest existing file (a typo is fixable
// in one edit), and a capped list of the site's linkable files so the agent
// can pick the right target without spending a turn on list_files.
func brokenLinkMessage(rawVal, resolved string, fileSet map[string]bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "broken link %q (resolved to %q) — no such file in the site.", rawVal, resolved)
	linkable := linkableFiles(fileSet)
	if best := closestMatch(resolved, linkable); best != "" {
		fmt.Fprintf(&b, " Did you mean %q?", best)
	}
	if len(linkable) > 0 {
		fmt.Fprintf(&b, " Site files: %s.", capList(linkable, maxListedItems))
	}
	b.WriteString(" Point the href/src at an existing file or create the missing one.")
	return b.String()
}

// linkableFiles returns the sorted site files the proxy would actually serve:
// dotfiles (metadata sidecars), reserved "_" prefixes (e.g. _state/), and
// functions/ (reachable only via /api/, never as a static path) are excluded
// so the agent is never steered toward an unlinkable target.
func linkableFiles(fileSet map[string]bool) []string {
	out := make([]string, 0, len(fileSet))
	for f := range fileSet {
		if strings.HasPrefix(f, "_") || strings.HasPrefix(f, "functions/") || strings.HasPrefix(path.Base(f), ".") {
			continue
		}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}

// maxSuggestDistance bounds how fuzzy a did-you-mean match may be. Three
// edits catches the common agent mistakes (transposed letters, a dropped
// character, a wrong extension) without proposing unrelated files.
const maxSuggestDistance = 3

// closestMatch returns the candidate nearest to target by edit distance, or
// "" when nothing is plausibly the intended file. Comparison is
// case-insensitive so a casing mismatch (About.html vs about.html — distinct
// keys in S3) surfaces as a distance-zero suggestion.
func closestMatch(target string, candidates []string) string {
	best, bestDist := "", maxSuggestDistance+1
	lowTarget := strings.ToLower(target)
	for _, c := range candidates {
		d := levenshtein(lowTarget, strings.ToLower(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	if bestDist > maxSuggestDistance || bestDist >= len(lowTarget) {
		return ""
	}
	return best
}

// levenshtein is the classic two-row edit distance — small inputs only
// (file names), so no need for anything fancier.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}

// maxListedItems caps the file/id inventories embedded in lint messages so a
// large site can't balloon the fix prompt the agent receives.
const maxListedItems = 15

// capList joins items, truncating past limit with a "+N more" suffix.
func capList(items []string, limit int) string {
	if len(items) > limit {
		return strings.Join(items[:limit], ", ") + fmt.Sprintf(", +%d more", len(items)-limit)
	}
	return strings.Join(items, ", ")
}
