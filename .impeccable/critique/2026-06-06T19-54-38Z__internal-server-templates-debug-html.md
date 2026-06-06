---
target: internal/server/templates/debug.html
total_score: 22
p0_count: 1
p1_count: 0
timestamp: 2026-06-06T19-54-38Z
slug: internal-server-templates-debug-html
---
#### Design Health Score — debug.html (transcripts index)

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visual hierarchy | 2 | One H1, one prose line, identical rows — flat by design but no rank inside the row |
| 2 | Information rhythm | 1 | Card-grid of monotone tiles, no leading column, no scan anchor |
| 3 | Density | 1 | A transcripts list is a log; cards waste vertical room a table wouldn't |
| 4 | Typography | 3 | Tight scale, semibold time + /60 ISO, "View →" is fine |
| 5 | Color discipline | 3 | Clean. Single primary affordance, no Pulp misuse |
| 6 | Mono usage | 3 | LogKey in `<code>` inside ghost badge — correct |
| 7 | Empty state | 3 | Sober card, points forward in time; could be more diagnostic but honest |
| 8 | Accessibility | 2 | `<a>` wraps a card with mixed-weight children; no aria; `→` decorative-only without `aria-hidden` |
| 9 | Brand voice | 3 | Sober, no mascot, sentence-case — right register |
| 10 | Diagnostic fitness | 1 | Cannot scan 100 runs visually — no status column, no duration, no error pre-flag |
| **Total** | | **22/40** | **Acceptable — significant table-conversion needed** |

#### Anti-Patterns Verdict

**LLM assessment.** This is the slop-shaped one of the three diagnostic surfaces. "A list of cards with identical visual weight where every row says only a timestamp + a key" is exactly the diagnostic-page failure mode the rules name. It should be a table. The card-grid pattern is what every AI page-generator produces for "show me a list of N records" — converting to a real table with status/duration columns is the discipline move.

Mono usage is correct: `LogKey` in `<code>` inside a ghost badge, sentence-case `WhenLabel`, /60 ISO timestamp.

**Deterministic scan.** Clean. Exit 0, no findings.

#### Priority Issues

- **[P0] Card grid where a table belongs.** Cannot scan 100 runs. No status column, no duration, no error pre-flag. Each row is click-to-find-out. **Fix:** convert to `table table-zebra` with columns `when` (sentence) / `when ISO` (mono /60 below) / `kind` (LogKey, mono) / `status` (badge) / `duration` (mono) / `tool calls` (count) / actions. Match files.html's lowercase heads.
  **Command:** `/impeccable distill internal/server/templates/debug.html`

- **[P2] Empty state doesn't teach.** A new admin reading "The first one will appear after your next build or edit" learns nothing about *what* a transcript captures. **Fix:** under the card body, add a short list — "Transcripts capture: every tool the agent called, before/after of every file, token usage, errors."
  **Command:** `/impeccable onboard internal/server/templates/debug.html`

- **[P3] `<a>` wraps mixed-weight card content with no aria.** Screen readers read the whole flex-row as one anchor name. The `→` is decorative without `aria-hidden`. **Fix:** add `aria-label="View transcript {{ .WhenLabel }}"` to the link and `aria-hidden="true"` to the arrow span.
  **Command:** `/impeccable harden internal/server/templates/debug.html`

#### Persona Red Flags

- **Alex (super-admin):** one click per row to learn status. Hard no.
- **Riley (long values):** badge-ghost doesn't wrap a `<code>`; a long LogKey will overflow ungracefully.

#### Minor Observations

- "View →" is the only colored thing in the row and it's the last thing; the eye lands on it after the timestamp, which means the actual scan target (status) doesn't exist.
- `border-dashed` empty card is correct system-wide.
