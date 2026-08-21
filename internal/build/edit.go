package build

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/textedit"
)

// The edit-prompt format strings the agent sees on every edit submission.
// Kept as sibling .md files so they read as plain text and stay easy to
// tweak; each is a fmt.Sprintf format string with the placeholders noted on
// its EditPrompt switch arm.

//go:embed edit_site_prompt.md
var editSitePromptFmt string // placeholders: %s = user prompt

//go:embed edit_page_prompt.md
var editPagePromptFmt string // placeholders: %s = page name, %s = user prompt

// editPrefetchTotalCap caps the total bytes of HTML page content we'll inline
// into seeded read_file responses. Beyond this, we let the agent issue its
// own read_file calls so we don't blow the context window on a sprawling site.
const editPrefetchTotalCap = 32 * 1024

// SplitFilesByKind partitions a slug's file list into editable HTML pages
// versus uploaded assets. Sidecars and unknown files are dropped from both.
func SplitFilesByKind(files []string) (pages, assets []string) {
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, "."):
			// sidecars like .topbanana.json
		case strings.HasPrefix(f, "assets/"):
			assets = append(assets, f)
		case strings.HasSuffix(f, ".html"):
			pages = append(pages, f)
		}
	}
	return pages, assets
}

// EditableFiles returns the files an editing tool may rewrite, in stable
// order: the HTML pages plus the text assets a site carries. Distinct from
// SplitFilesByKind, which OptimizeCSS relies on to mean "HTML only" — feeding
// a stylesheet into the Tailwind content scan, or letting the page rewriter
// inject a <link> tag into one, are both wrong.
func EditableFiles(files []string) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		if textedit.IsTextAsset(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// EditSeeds returns synthetic tool-call seeds for an edit invocation: a
// list_files seed (so the agent doesn't need that round-trip) and read_file
// seeds for as much of the site as fits in editPrefetchTotalCap.
//
// Seeding used to prefetch only pages the prompt named, which meant a prompt
// naming no page at all — "make the images bigger" — seeded nothing but the
// file list. The agent then read pages at random, guessed at edit_file
// old_text, failed the byte match twice, and re-read: four iterations to reach
// the state it should have started in, working from a mental model that never
// included the file governing the change. Most sites fit well inside the cap,
// so prefetch by priority until the budget runs out rather than by name alone.
//
// Errors are swallowed and logged: seeding is an optimization, never a gating
// step. If we can't list the bucket, the agent proceeds without seeds.
func (svc *Service) EditSeeds(ctx context.Context, slug, prompt string) []agent.SeedToolCall {
	files, err := svc.store.List(ctx, slug)
	if err != nil {
		slog.Warn("edit.seeds.list_failed", "slug", slug, "err", err)
		return nil
	}
	editable := EditableFiles(files)
	if len(editable) == 0 {
		return nil
	}

	seeds := make([]agent.SeedToolCall, 0, 1+len(editable))
	// The seeded listing must match what the live list_files tool answers, or
	// the agent's first real call contradicts its own history.
	seeds = append(seeds, agent.SeedToolCall{
		Name:     "list_files",
		Args:     map[string]any{},
		Response: map[string]any{"files": editable},
	})

	ordered := seedOrder(editable, prompt)
	total := 0
	capped := false
	for _, path := range ordered {
		obj, err := svc.store.Read(ctx, slug, path)
		if err != nil || obj == nil {
			slog.Warn("edit.seeds.read_failed", "slug", slug, "page", path, "err", err)
			continue
		}
		if total+len(obj.Content) > editPrefetchTotalCap {
			// Keep going: a later, smaller file may still fit, and a
			// stylesheet the agent can't see is the failure mode this
			// prefetch exists to prevent.
			capped = true
			continue
		}
		total += len(obj.Content)
		totalLines := 0
		if obj.Content != "" {
			totalLines = strings.Count(obj.Content, "\n") + 1
		}
		seeds = append(seeds, agent.SeedToolCall{
			Name: "read_file",
			Args: map[string]any{"path": path},
			Response: map[string]any{
				"content":     agent.NumberLines(obj.Content, 1),
				"total_lines": totalLines,
			},
		})
	}

	slog.Info("edit.prefetch", "slug", slug, "editable", len(editable), "seeded_reads", len(seeds)-1, "bytes", total, "capped", capped)
	return seeds
}

// seedOrder ranks the editable files by how much a prefetch of each is worth,
// so that when the budget runs out it runs out on the least useful file:
//
//  1. pages the prompt names outright — the user pointed at them
//  2. index.html, the mandatory entry point and the usual target
//  3. the site's own stylesheets, which govern every page at once and are the
//     highest signal per byte on a site that carries one
//  4. every remaining page
//  5. everything else (svg, md, txt, json, xml)
//
// Each file appears once, at its best rank.
func seedOrder(editable []string, prompt string) []string {
	pages := make([]string, 0, len(editable))
	for _, f := range editable {
		if strings.HasSuffix(f, ".html") {
			pages = append(pages, f)
		}
	}

	seen := make(map[string]bool, len(editable))
	out := make([]string, 0, len(editable))
	take := func(candidates []string, keep func(string) bool) {
		for _, f := range candidates {
			if seen[f] || !keep(f) {
				continue
			}
			seen[f] = true
			out = append(out, f)
		}
	}
	always := func(string) bool { return true }

	take(pagesNamedInPrompt(pages, prompt), always)
	take(editable, func(f string) bool { return f == "index.html" })
	take(editable, func(f string) bool { return strings.HasSuffix(f, ".css") })
	take(pages, always)
	take(editable, always)
	return out
}

// pagesNamedInPrompt returns the subset of pages whose full name (e.g.
// "about.html") or basename (e.g. "about") appears as a whole word in prompt.
// The candidate set is built from the actual file list, so a stray "home" in
// prose only matches when home.html truly exists.
func pagesNamedInPrompt(pages []string, prompt string) []string {
	if len(pages) == 0 || prompt == "" {
		return nil
	}

	tokens := make([]string, 0, 2*len(pages))
	byToken := make(map[string]string, 2*len(pages))
	for _, p := range pages {
		base := strings.TrimSuffix(p, ".html")
		for _, t := range []string{p, base} {
			lower := strings.ToLower(t)
			if _, seen := byToken[lower]; seen {
				continue
			}
			byToken[lower] = p
			tokens = append(tokens, regexp.QuoteMeta(lower))
		}
	}
	if len(tokens) == 0 {
		return nil
	}

	re, err := regexp.Compile(`(?i)\b(?:` + strings.Join(tokens, "|") + `)\b`)
	if err != nil {
		slog.Warn("edit.seeds.regex_failed", "err", err)
		return nil
	}

	seen := make(map[string]bool, len(pages))
	out := make([]string, 0, len(pages))
	for _, m := range re.FindAllString(prompt, -1) {
		page, ok := byToken[strings.ToLower(m)]
		if !ok || seen[page] {
			continue
		}
		seen[page] = true
		out = append(out, page)
	}
	return out
}

// EditPrompt constructs the user-facing prompt for an edit invocation. page
// narrows the scope: empty page → site-wide; non-empty → that file.
func EditPrompt(prompt, page string) string {
	if page == "" {
		return fmt.Sprintf(editSitePromptFmt, prompt)
	}

	return fmt.Sprintf(editPagePromptFmt, page, prompt)
}

// EditPromptWithHistory is EditPrompt plus a short summary of what the user
// already asked for on this site. Use it for user-initiated edits; the
// lint-fix and polish passes want the bare prompt, since they aren't requests
// and their history would read to the agent as though the user had made one.
//
// Best-effort: no history, or an unreadable transcript, yields the same prompt
// EditPrompt would have produced.
func (svc *Service) EditPromptWithHistory(ctx context.Context, slug, prompt, page string) string {
	base := EditPrompt(prompt, page)
	block := FormatRunHistory(svc.RecentRuns(ctx, slug, historyRuns), prompt, time.Now().UTC())
	if block == "" {
		return base
	}
	return base + "\n\n" + block
}

// OwnStylesheets returns the stylesheets a site authored for itself, newest
// listing order, or nil when it has none. Presence of one selects the
// bring-your-own-CSS styling regime for the agent (see agent.BuildContext).
//
// Detection keys on what the site actually carries, never on the `/app.css`
// link in <head>: OptimizeCSS injects that tag into every page unconditionally
// and lint requires it, so its presence says nothing about how a site is
// styled. Reading it as a signal is what let a hand-authored site be handed
// DaisyUI guidance it could not use.
//
// Best-effort: a list failure yields the platform regime, which is both the
// safe default and the prior behaviour.
func OwnStylesheets(ctx context.Context, st *store.Store, slug string) []string {
	files, err := st.List(ctx, slug)
	if err != nil {
		slog.Warn("regime.list_failed", "slug", slug, "err", err)
		return nil
	}
	var sheets []string
	for _, f := range EditableFiles(files) {
		if strings.HasSuffix(f, ".css") {
			sheets = append(sheets, f)
		}
	}
	return sheets
}
