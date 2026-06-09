package agent

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/topbanana/internal/templates"
)

// This file owns assembly of the agent's system instruction: the per-template
// declarative-check summary, the cache-stable layering of base prompt +
// addenda + skeleton/example notices, and the per-build context block.
// Extracted from agent.go.

// formatTemplateChecks renders a template's declarative Checks as a short
// upfront requirement list so the agent's first pass already targets the
// invariants the lint loop will later assert. Without this the model only
// learns about a missing <h1> or <form> through a retry round-trip — every
// avoided retry skips a fresh ~5–7K-token prefix resend.
func formatTemplateChecks(checks []templates.Check) string {
	if len(checks) == 0 {
		return ""
	}
	lines := []string{"Your output will be validated against these requirements (the lint loop asserts them after every build):"}
	for _, c := range checks {
		if c.File == "" || len(c.MustContain) == 0 {
			continue
		}
		needles := make([]string, 0, len(c.MustContain))
		for _, n := range c.MustContain {
			needles = append(needles, fmt.Sprintf("`%s`", n))
		}
		line := fmt.Sprintf("- %s must contain %s", c.File, strings.Join(needles, " and "))
		if c.Message != "" {
			line += " — " + c.Message
		}
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// buildInstruction layers the per-template addendum on top of the base system
// prompt and adds a one-liner whenever the template ships skeleton files, so
// the agent knows to inspect the existing filesystem before writing.
//
// Order matters for prompt caching. Providers that cache automatically
// (OpenAI, DeepSeek, Gemini, Grok, Groq, Moonshot via OpenRouter) reuse
// whatever stable prefix the request opens with, so we lay the parts down
// stablest-first: base prompt (every build), functions addendum (shared by
// every functions-enabled template), per-template addendum (template-stable),
// skeleton notice (template-stable), examples notice (template-stable
// content, but the names list is template-determined), build context (the
// per-build meta block — date, slug, mode), then attachments notice (the
// only per-request variable). Any reordering of these blocks invalidates
// the cache for every build that follows.
func buildInstruction(tmpl *templates.SiteTemplate, attachments []Attachment, bctx BuildContext) string {
	parts := []string{systemPrompt}
	if tmpl != nil {
		if tmpl.EnablesFunctions {
			parts = append(parts, functionsPrompt)
		}
		if tmpl.PromptAddendum != "" {
			parts = append(parts, tmpl.PromptAddendum)
		}
		if checks := formatTemplateChecks(tmpl.Checks); checks != "" {
			parts = append(parts, checks)
		}
		if len(tmpl.Skeleton) > 0 {
			parts = append(parts, "A starter skeleton has already been written for this site and pre-loaded into your conversation history via seeded list_files / read_file (and list_functions / read_function for handlers). Extend or refine the existing files rather than starting from scratch.")
		}
		if len(tmpl.Examples) > 0 {
			names := make([]string, 0, len(tmpl.Examples))
			for n := range tmpl.Examples {
				names = append(names, n)
			}
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("Reference exemplars for this template were pre-loaded via read_example calls: %s. Use them as inspiration for layout, type hierarchy, and DaisyUI component composition — do not copy markup verbatim. The user's content comes from the prompt and any attachments, not from these examples.", strings.Join(names, ", ")))
		}
	}
	if block := formatBuildContext(bctx); block != "" {
		parts = append(parts, block)
	}
	if len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, a.Name)
		}
		parts = append(parts, fmt.Sprintf("The user attached the following reference files (markdown or HTML): %s. Their contents were pre-loaded into your conversation history via read_attachment calls. Treat them as authoritative source for page copy unless the user's prompt says otherwise.", strings.Join(names, ", ")))
	}
	return strings.Join(parts, "\n\n")
}

// formatBuildContext renders the per-build meta block. An entirely zero-value
// BuildContext returns "" so unit tests and any caller that has not migrated
// yet do not get a garbage block. A populated Now alone is enough to render
// the date line; Slug/SiteURL render together when both are set.
func formatBuildContext(bctx BuildContext) string {
	lines := []string{}
	if !bctx.Now.IsZero() {
		lines = append(lines, "- Today: "+bctx.Now.Format("Monday, 2006-01-02"))
	}
	if bctx.Slug != "" && bctx.SiteURL != "" {
		lines = append(lines, fmt.Sprintf("- Site: %s at %s", bctx.Slug, bctx.SiteURL))
	} else if bctx.Slug != "" {
		lines = append(lines, "- Site: "+bctx.Slug)
	}
	mode := "initial build (skeleton seeded — extend it)"
	if bctx.IsEdit {
		mode = "follow-up edit (extend or surgically modify existing files; prefer edit_file / replace_lines over rewriting whole pages)"
	}
	// Mode is meaningful only when there is enough other context to anchor
	// it — without slug or date the agent has nothing to attach it to.
	if len(lines) > 0 {
		lines = append(lines, "- Mode: "+mode)
	}
	if len(lines) == 0 {
		return ""
	}
	return "Build context:\n" + strings.Join(lines, "\n")
}
