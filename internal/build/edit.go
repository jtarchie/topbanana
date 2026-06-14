package build

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/jtarchie/topbanana/internal/agent"
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

// EditSeeds returns synthetic tool-call seeds for an edit invocation: always
// a list_files seed (so the agent doesn't need that round-trip), and a
// read_file seed for each existing HTML page mentioned by name in the user's
// prompt, capped at editPrefetchTotalCap total bytes.
//
// Errors are swallowed and logged: seeding is an optimization, never a gating
// step. If we can't list the bucket, the agent proceeds without seeds.
func (svc *Service) EditSeeds(ctx context.Context, slug, prompt string) []agent.SeedToolCall {
	files, err := svc.store.List(ctx, slug)
	if err != nil {
		slog.Warn("edit.seeds.list_failed", "slug", slug, "err", err)
		return nil
	}
	pages, _ := SplitFilesByKind(files)
	if len(pages) == 0 {
		return nil
	}

	seeds := make([]agent.SeedToolCall, 0, 1+len(pages))
	seeds = append(seeds, agent.SeedToolCall{
		Name:     "list_files",
		Args:     map[string]any{},
		Response: map[string]any{"files": pages},
	})

	matched := pagesNamedInPrompt(pages, prompt)
	total := 0
	capped := false
	for _, page := range matched {
		obj, err := svc.store.Read(ctx, slug, page)
		if err != nil || obj == nil {
			slog.Warn("edit.seeds.read_failed", "slug", slug, "page", page, "err", err)
			continue
		}
		if total+len(obj.Content) > editPrefetchTotalCap {
			capped = true
			break
		}
		total += len(obj.Content)
		totalLines := 0
		if obj.Content != "" {
			totalLines = strings.Count(obj.Content, "\n") + 1
		}
		seeds = append(seeds, agent.SeedToolCall{
			Name: "read_file",
			Args: map[string]any{"path": page},
			Response: map[string]any{
				"content":     agent.NumberLines(obj.Content, 1),
				"total_lines": totalLines,
			},
		})
	}

	slog.Info("edit.prefetch", "slug", slug, "pages", len(pages), "matched", len(matched), "seeded_reads", len(seeds)-1, "bytes", total, "capped", capped)
	return seeds
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
