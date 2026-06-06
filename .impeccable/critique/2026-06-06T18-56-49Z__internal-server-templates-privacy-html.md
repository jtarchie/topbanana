---
target: internal/server/templates/privacy.html
total_score: 33
p0_count: 0
p1_count: 1
timestamp: 2026-06-06T18-56-49Z
slug: internal-server-templates-privacy-html
---
#### Design Health Score — privacy.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Same H1+13-H2 pattern; "The short version" lead helps |
| 2 | Match System / Real World | 4 | "That's the whole list. No advertisers, no analytics resellers." Strong |
| 3 | User Control and Freedom | 3 | No anchors |
| 4 | Consistency and Standards | 3 | `<code>` pills break monotony helpfully |
| 5 | Error Prevention | 4 | n/a |
| 6 | Recognition Rather Than Recall | 3 | TL;DR section is genuinely useful; still no TOC |
| 7 | Flexibility and Efficiency | 3 | Same anchor gap |
| 8 | Aesthetic and Minimalist | 3 | Identical to terms |
| 9 | Error Recovery | 4 | n/a |
| 10 | Help and Documentation | 4 | Names actual systems: WebAuthn, Let's Encrypt, `.tar.zst` |
| **Total** | | **33/40** | **Good — slightly stronger than terms thanks to TL;DR** |

#### Anti-Patterns Verdict

**LLM assessment.** Same typographic care as terms.html. The TL;DR ("The short version") IS the page's strongest IA move — surface it more aggressively. The duplicated `alert-success` shield block from terms.html appears here too, with the same shield icon + same alert variant + same opening message style. They're saying genuinely different things (terms = IP, privacy = data) but the visual treatment hides that.

Inline `<code>` pills use `text-sm` while body is `text-base` — causes a visible baseline hop mid-paragraph.

**Detector:** clean (0 findings).

#### Priority Issues

- **[P1] Same anchor/TOC gap as terms, and it matters more here.** GDPR/CCPA "I want to delete my data" is the #1 reason anyone reads a privacy page. **Fix:** IDs + jump nav; link "Your controls" and "Data retention" prominently from the top callout.
  **Command:** `/impeccable layout internal/server/templates/privacy.html`

- **[P2] "The short version" should be the lede, not section #N.** TL;DR is genuinely better than the doc. **Fix:** promote the `<ul>` from inside "The short version" to live above the long-form section. Drop the duplicate `alert-success` callout — the short version replaces it.
  **Command:** `/impeccable distill internal/server/templates/privacy.html`

- **[P3] Inline `<code>` pills baseline-hop.** `font-mono text-sm bg-base-200 px-1 rounded` inside `text-base` paragraph drops 2px mid-line. **Fix:** drop `text-sm` from inline `<code>` or set `text-[0.9em]` so it scales relative.
  **Command:** `/impeccable typeset internal/server/templates/privacy.html`

#### Persona Red Flags

- **Sam (a11y):** same as terms — flat list of 13 same-shaped headings.
- **Mira (non-coder reading privacy):** lands from footer wanting "do they read my prompts?", TL;DR answers it. But "What we collect" buries "Prompts" as item 3 of 5 — should probably be its own H2 since it's the load-bearing concern for an AI product.

#### Minor Observations

- "Sites are public by default" is a great H2 — concrete and specific. Use as model.
- `<code>{slug}.topbanana.dev</code>` uses literal `{slug}` braces; consider `<var>slug</var>.topbanana.dev` for semantic correctness.
- Footer cross-link to /terms mirrors terms.html — good consistency.
