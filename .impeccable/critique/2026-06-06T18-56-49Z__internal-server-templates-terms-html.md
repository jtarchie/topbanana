---
target: internal/server/templates/terms.html
total_score: 32
p0_count: 0
p1_count: 1
timestamp: 2026-06-06T18-56-49Z
slug: internal-server-templates-terms-html
---
#### Design Health Score — terms.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Clear H1, but 13 identical H2s in a row become a wall |
| 2 | Match System / Real World | 4 | "Friendly, cheeky, capable" actually carries ("is zero", "please don't try") |
| 3 | User Control and Freedom | 3 | No skip-to-section / anchor permalinks |
| 4 | Consistency and Standards | 3 | H2 → P rhythm is uniform and undifferentiated |
| 5 | Error Prevention | 4 | n/a for a static doc; no destructive surfaces |
| 6 | Recognition Rather Than Recall | 2 | No TOC; user must scan 13 H2s for one section |
| 7 | Flexibility and Efficiency | 3 | No anchor URLs to share specific sections |
| 8 | Aesthetic and Minimalist | 3 | Good measure, good leading, no rhythm differentiation between sections |
| 9 | Error Recovery | 4 | n/a — no input |
| 10 | Help and Documentation | 3 | "See also" footer block is a nice touch; could mirror as semantic nav |
| **Total** | | **32/40** | **Good — solid baseline, polish wins remain** |

#### Anti-Patterns Verdict

**LLM assessment.** Better than baseline AI slop. The `max-w-3xl` container is ~768px which at body type is ~70-75ch — on-spec for the 65-75ch target. Real typographic care: `leading-relaxed` on prose, `tracking-tight` on H2s, semibold `<strong>` for list-item leads. Voice carries through formal sections ("which, for a free account, is zero"; "please don't try"). No glassmorphism, no side-stripes, no gradient text, no numbered scaffolding. Em-dashes are HTML entities used correctly as punctuation, not as AI-tell scaffolding.

The tell: 13 H2s of identical size/weight/spacing in a row is the classic "long paragraphs with no rhythm" pattern, just disguised as a list of headings. There is no anchor on any H2 — the single most useful affordance for legal pages.

The opening `alert-success` "shield" callout reuses an a11y status pattern for a non-status message. `alert-success` implies confirmation of an action; nothing was just confirmed.

**Detector:** clean (0 findings).

#### Priority Issues

- **[P1] No anchors / TOC on a 13-section legal page.** Users come with a specific question; linear scanning is the wrong affordance. **Fix:** add `id="acceptable-use"` etc. on each H2; prepend a `<nav aria-label="On this page">` with a simple `<ul>` of jump links after the header.
  **Command:** `/impeccable layout internal/server/templates/terms.html`

- **[P2] H2 rhythm is uniform to noise.** 13 identical H2s = striped wall. **Fix:** group into 3-4 thematic clusters with thin `border-t border-base-200 pt-10` separators, or escalate one H2 per cluster to title-weight for landmarks.
  **Command:** `/impeccable typeset internal/server/templates/terms.html`

- **[P3] The opening `alert-success` "shield" is a misused status pattern.** **Fix:** convert to a plain `<aside class="rounded-box border border-base-300 bg-base-200 p-4">` callout. Ladder Rule depth, no semantic alert, no shield icon (reserved for actual confirmations).
  **Command:** `/impeccable distill internal/server/templates/terms.html`

#### Persona Red Flags

- **Sam (a11y):** no skip-to-content target; per-section anchors absent; 13 same-shaped headings with no grouping.
- **Mira (non-coder reading legal):** lands wanting "do they read my prompts?", has to scan 13 H2s for the answer.

#### Minor Observations

- `&mdash;` appears 4× inline; some could be commas with same break.
- "See also" footer is nice; mirror as `<nav aria-label="Related">`.
- Cross-link to /privacy uses `link link-hover font-medium` — good, distinguished from inline body link style.
