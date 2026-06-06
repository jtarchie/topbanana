---
target: internal/server/templates/error.html
total_score: 24
p0_count: 1
p1_count: 1
timestamp: 2026-06-06T18-56-49Z
slug: internal-server-templates-error-html
---
#### Design Health Score — error.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Status number is big, title smaller, tagline muted |
| 2 | Match System / Real World | 2 | Tagline is generic ("Something went wrong"-class) |
| 3 | User Control and Freedom | 2 | One CTA only; no "back", no request ID, no support hint |
| 4 | Consistency and Standards | 2 | No global nav / footer — user is stranded from orientation |
| 5 | Error Prevention | n/a | n/a — it IS the error |
| 6 | Recognition Rather Than Recall | 3 | Status code is large, but it's also Lemonade Pulp (decoration) |
| 7 | Flexibility and Efficiency | 2 | Single escape; no browser-back, no contextual recovery |
| 8 | Aesthetic and Minimalist | 3 | Card sized well, gap generous |
| 9 | Error Recovery | 2 | "Take me home" is the only escape; goes to / (marketing landing), not the user's workspace |
| 10 | Help and Documentation | 2 | No "what page were you trying to reach", no request ID, no contact link |
| **Total** | | **24/40** | **Acceptable — significant fixes needed, P0 mascot + IA** |

#### Anti-Patterns Verdict

**LLM assessment — and this is the headline finding for the trio.**

The page renders a **monkey** SVG (lines 10-35) — a different cartoon mascot than the banana. The comment on line 11 literally says `<!-- friendly monkey -->`. The palette uses invented hex values (`#fcd9b6`, `#fde2c4`, `#9a6a3c`) that aren't in the documented `mascot-*` set. And then, doubling the violation: lines 31-34 hang a tiny banana off the monkey with the comment `<!-- a little banana, because why not -->` — the actual smoking gun in the source.

This violates PRODUCT.md ("the brand mark is a smiling banana") and DESIGN.md (Component philosophy §5: "Modifications: no facial swaps, no hand/arm additions … no rotations, no color shifts. If the surface needs a different mood, use a different SVG and reserve this one for Top Banana itself." — but that doesn't authorize *inventing* a different mascot).

Plus:
- `text-6xl` (60px) on the status number violates the Tight Scale Rule's documented top step.
- `font-bold` (700) is the only place in the trio that breaks the 400/500/600 weight ladder.
- `text-primary` (Lemonade Pulp) on a *failure* status number is tonally backwards — Pulp is the verb-color for primary actions, not decoration of broken states.
- No `{{ template "brand" . }}`, no `{{ template "footer" . }}` — user is stranded from the global nav. On a 404 the nav is often what they want.
- "Take me home" goes to `/` which is the marketing landing, not the user's workspace or `/apps`. For a logged-in user this is the wrong "home."
- No `role="alert"` / `aria-live` so an SPA-style XHR swap wouldn't announce the failure.

**Detector:** clean (0 findings). The detector catches CSS shapes, not mascot identity or IA gaps.

#### Priority Issues

- **[P0] Mascot violation: the monkey isn't the brand mark.**
  - **Why it matters:** the brand is the banana; there is no second mascot. The `because why not` comment in the source confirms it's drift, not a brand decision.
  - **Fix:** replace the monkey SVG with either (a) the canonical banana SVG at larger scale with a quiet expression (no animation), or (b) a status-mark glyph (broken-link icon for 404, server-stack icon for 5xx) and no mascot at all. Option (a) preserves identity; (b) removes the question entirely.
  - **Suggested command:** `/impeccable harden internal/server/templates/error.html`

- **[P1] No recovery path beyond a single "home" link that goes to the wrong "home."**
  - **Why it matters:** a 404 user wants "back where I was" or "search apps"; a 5xx user wants "try again" or "report this." For logged-in users, `/` is the marketing landing, not their site. "Home" is contextually wrong.
  - **Fix:** add a secondary `<a href="javascript:history.back()">` (or a form-button) as a ghost. Plumb `.IsLoggedIn` and switch the "home" target between `/` and `/apps`. Optionally surface a muted `Status code · request ID` line. Switch tagline by status family: 4xx = "we couldn't find that"; 5xx = "we broke something, not you."
  - **Suggested command:** `/impeccable clarify internal/server/templates/error.html`

- **[P2] No global nav / footer; user is stranded.**
  - **Why it matters:** skipping `brand` and `footer` strips the only orientation. On 404 the nav is often what they want — to navigate elsewhere.
  - **Fix:** include `{{ template "brand" . }}` and `{{ template "footer" . }}`. Drop the `grid place-items-center` body in favor of `flex flex-col min-h-screen` (mirrors how the legal pages and the rest of the app are structured).
  - **Suggested command:** `/impeccable layout internal/server/templates/error.html`

#### Persona Red Flags

- **Sam (a11y):** no `<main id="main">` skip target, no `role="alert"` / `aria-live` so an SPA XHR swap wouldn't announce the failure. The status number isn't semantically tied to the heading.
- **Riley (hitting error.html via stale link):** clicks a stale link to a subdomain page that no longer exists. Sees a monkey, "404", "Take me home" which goes to `/` — the marketing landing, not their workspace. Can't tell if the slug doesn't exist or the page within the slug doesn't exist.

#### Minor Observations

- `<title>` is just `{{ .Status }} — Top Banana` — should be `{{ .Status }} · {{ .Title }} — Top Banana` for browser-history scannability ("404 · Not Found — Top Banana").
- `bg-base-200` on body with `bg-base-100` card IS the Ladder Rule applied correctly — one of the things this file gets right.
- Card uses `card bg-base-100 border border-base-300` — on-pattern with the rest of the system. The chrome around the mascot violation is correct.
