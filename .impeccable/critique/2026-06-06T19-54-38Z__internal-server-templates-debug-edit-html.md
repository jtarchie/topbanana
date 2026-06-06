---
target: internal/server/templates/debug_edit.html
total_score: 29
p0_count: 0
p1_count: 2
timestamp: 2026-06-06T19-54-38Z
slug: internal-server-templates-debug-edit-html
---
#### Design Health Score — debug_edit.html (single transcript)

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visual hierarchy | 2 | Three sections at equal weight; "Run details" buries the most diagnostic facts (status, error) |
| 2 | Information rhythm | 2 | DL + tool-call list + collapsibles is varied, but every section feels equally important |
| 3 | Density | 3 | DL is genuinely dense; tool-call rows are compact one-liners |
| 4 | Typography | 3 | Mono on the right values, /60 on labels — correct |
| 5 | Color discipline | 3 | Status & change badges are semantic; one primary link |
| 6 | Mono usage | 3 | Paths, tools, durations, tokens — all mono. Status text isn't. Correct |
| 7 | Empty state | 4 | The "no tool calls" / "no file changes" copy is genuinely diagnostic — tells the admin what that means |
| 8 | Accessibility | 2 | `<details>` summaries cram a button inside the summary — focus-order traps |
| 9 | Brand voice | 4 | "this is the smoking gun" — exactly right register for the audience |
| 10 | Diagnostic fitness | 3 | The before/after diff + cache-check button is the real win. But it's not a diff — it's two `<pre>` |
| **Total** | | **29/40** | **Good — strongest of the trio** |

#### Anti-Patterns Verdict

**LLM assessment.** The strongest page of the diagnostic trio. The authored copy ("this is the smoking gun", "almost always means it returned a chat response instead of writing files") is the smoking gun for *real care* — no template-spitter writes those sentences. The DL + tool-call list + collapsibles structure is genuinely varied. Empty states teach. This is what a forensics surface should look like.

The two weak spots are visual rank (Status/Error get buried by their position in the dl) and the before/after rendering (two raw `<pre>` blocks instead of a real diff).

**Deterministic scan.** Clean. Exit 0, no findings.

#### Priority Issues

- **[P1] Run details list buries the load-bearing facts.** Status and Error are what the admin came for; they sit in row 2 and row 11 of the dl. **Fix:** hoist `Status` + `FinalStatus` + (if present) `Error` into a small ribbon above the dl — single line, status badge + duration + (red) error message clamped to 1-line with `title=` for full text. Keep the full dl below for everything else.
  **Command:** `/impeccable clarify internal/server/templates/debug_edit.html`

- **[P1] Before/After is two `<pre>` blocks, not a diff.** For text files this is the single most fixable diagnostic shortcoming — admins eyeball-diffing 200-line HTML is the bug. **Fix:** render a unified-or-side-by-side line-level diff server-side (`internal/build` likely has the diff already from `Service.Start`). At minimum colorize added/removed lines in the existing pres via a tiny prefix scan (`+ ` green-ish, `- ` red-ish on the muted /70 line) before falling back to raw.
  **Command:** `/impeccable overdrive internal/server/templates/debug_edit.html`

- **[P2] Cache-check button lives inside `<summary>` with `stopPropagation`.** Fragile pattern; some browsers still toggle. **Fix:** move the button into the `collapse-content` near `cache-check-result`, label it "Check cache for this path", drop the `event.stopPropagation`.
  **Command:** `/impeccable harden internal/server/templates/debug_edit.html`

#### Persona Red Flags

- **Sam (a11y):** `<details>` summaries with embedded buttons are focus-order traps. Inline-button-inside-summary is a known gotcha.
- **Riley (very long paths):** `Path` in the `<summary>` flex-row is not `min-w-0` — a 200-char path will push the badges off-screen.

#### Minor Observations

- `bg-base-100` on a `<pre>` inside a `bg-base-100` card means the `<pre>` has no contrast — should be `bg-base-200` to read as a code block.
- `Tokens` row uses `·` separators inline with /60 parens — nice, keep.
- `card-title text-lg` on "Run details" plus `text-lg font-semibold` on the next two H2s means three "lg" headings — one of them (probably "Run details") could be `text-base` to demote it visually.
