---
target: internal/server/templates/apps.html
total_score: 25
p0_count: 0
p1_count: 2
timestamp: 2026-06-06T17-53-35Z
slug: internal-server-templates-apps-html
---
#### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | "Edited Xm ago" gives recency; no row-level "live / building / failed" signal; no app count |
| 2 | Match System / Real World | 3 | "Your apps," "New app," "Open ↗" all plain-English; `⋮` Unicode is the one piece of jargon |
| 3 | User Control and Freedom | 3 | Confirm modal cancels and ESCs cleanly; no undo on delete (platform posture, not this page's bug) |
| 4 | Consistency and Standards | 2 | Header CTA `btn-primary btn-sm` collapses to the same height as in-row `btn-ghost btn-sm`; landing's Create site and workspace's Apply both went default size |
| 5 | Error Prevention | 2 | Delete from the list is 3 clicks with no type-the-slug ceremony; manage.html now uses both modal + type-slug; the list-row gate is softer than the more-deliberate page |
| 6 | Recognition Rather Than Recall | 3 | Title + edited + description + slug + domain is a lot to scan; for untitled apps the slug duplicates between H2 and footer |
| 7 | Flexibility and Efficiency | 2 | No search, no sort, no count; fine at 3 apps, painful at 50 |
| 8 | Aesthetic and Minimalist Design | 2 | Five distinct text/affordance regions per row; dashed empty-state card is on-rule but reads slightly affected next to the sober chrome |
| 9 | Error Recovery | 3 | Over-quota alert is excellent: counts, cap, and consequence boundary all named |
| 10 | Help and Documentation | 2 | No "what is a slug?" affordance; first-timers don't know what the mono string under the title is |
| **Total** | | **25/40** | **Acceptable — significant improvements before users are happy** |

#### Anti-Patterns Verdict

**LLM assessment.** Not slop overall. The page is restrained, the copy is specific, the alerts are tight. But the tells worth naming:

- **Two overlapping click targets is the central tell.** Line 31's `<a href="/workspace/{{ .Name }}" class="block p-4 pr-32">` wraps the row interior, and inside the `absolute right-3` overlay (line 44) sits a *second* `<a href="{{ .URL }}">` at line 45. The overlay is a sibling of the row link, so the Open ↗ doesn't actually steal — but only because the structure carefully puts the overlay outside the anchor. The `pr-32` reserve (128px) is a magic-number guard against overlap, which is the kind of brittle margin that bites on long titles in narrow viewports. This is the "AI suggested an obvious pattern, the author rescued it with positioning" texture.
- **`has-[.dropdown:focus-within]:z-20`** at line 30. The arbitrary `:z-20` violates the Named Depth Rule we just added in the workspace pass. There's a `--z-dropdown` token waiting to be used.
- **`⋮` Unicode at line 47** is the one place this page should hand-author SVG. DESIGN.md is explicit about hand-authored SVG art for the brand mark; PRODUCT.md's feedback memory says "always hand-author SVG icons/art instead of using emoji glyphs." The vertical ellipsis renders inconsistently across platforms (different baseline, different width) and at `btn-square btn-sm` the optical centering is platform-dependent. A 16px inline SVG (three dots, current-color fill) would be both rule-compliant and more reliable.
- **Dashed empty-state card** is documented as the right "no content yet" affordance in DESIGN.md. So it's not slop. But **the empty state has no mascot**, which is the missed brand opportunity. Per PRODUCT.md, "the mascot does the smiling" — first-time arrival is the single most pride-loaded moment in this app's lifecycle, and the empty state is mascot-free chrome.
- **No mascot on the populated list either.** Probably correct (chrome stays sober), but the empty state is fair game.

**Deterministic scan.** `detect.mjs --json internal/server/templates/apps.html` exited 0 with `[]`. Zero findings. Once again the detector catches no CSS-shape bans; the issues are compositional (rank, calibration, scale).

**Visual overlays.** Unavailable — dev server isn't running and the file is a Go template.

#### Overall Impression

Competent list page with two structural drifts: the header CTA visually collapses against the in-row ghost actions (same `btn-sm` size), and the delete-from-list gate is now softer than the manage.html version we just hardened. The most-visited "your work lives here" surface should outrank everything, and the destructive action on a list — where spring-cleaning users move fastest — needs at least as much friction as the deliberate one-app-at-a-time page. Both fix in single edits.

The bigger arc: the page reads as "first 5 apps friendly, 50 apps hostile." No search, no sort, no count. The dashed empty-state card sits at the most pride-loaded moment in the user lifecycle and stays sober when the design system explicitly allows the mascot to smile. These aren't bugs; they're missed beats.

#### What's Working

1. **The over-quota alert (lines 21-25).** Counts the overage, names the cap, draws the consequence boundary clearly ("you can keep editing what's here, but you can't create new apps"). This is what honest microcopy looks like — concrete numbers, plain English, no scolding.
2. **Row interior as the navigation, actions as overlay** is the right IA call. A generous full-row target for the primary action (open workspace), secondary actions (Open ↗ and ⋮) lifted to the right without burying them. The implementation deserves polish; the concept is correct.
3. **Mono footer with `slug · custom-domain`** at `text-xs text-base-content/60`. On-rule (Mono token, `/60` caption). Gives the developer-secondary-audience exactly the wayfinding they need without inflicting it on the non-coder.

#### Priority Issues

- **[P1] Header CTA rank collapses against in-row ghosts.**
  - **Why it matters:** the page's most important action ("New app", line 12) is `btn btn-primary btn-sm`, same 2rem height as the per-row `btn-ghost btn-sm` (line 45) and the `⋮` icon button (line 47). The only differentiator is Lemonade Pulp fill. Returning users land here to *do something*; the primary should outrank everything. Landing's "Create site" and workspace's "Apply change" both went default size for exactly this reason.
  - **Fix:** drop `btn-sm` on the New app anchor: `<a href="/" class="btn btn-primary">New app</a>`. Matches the precedent now set across the system.
  - **Suggested command:** `/impeccable polish internal/server/templates/apps.html`

- **[P1] Delete from the list is undergated.**
  - **Why it matters:** three clicks to nuke a site (⋮ → Delete → modal OK), no type-the-slug ceremony. manage.html now requires both modal AND type-slug (defense in depth); the list is softer than the more-deliberate page. Spring-cleaning users move fast; the worst outcome is "I clicked the wrong row's ⋮."
  - **Fix:** add a type-the-slug step inside the confirm flow. Two paths: (a) escalate the confirm dialog to render a typed-confirmation input when `data-confirm-slug` is present; (b) route list-row delete through the manage page instead (which already has the strong gate). Option (a) is the smaller change and keeps the user on the list.
  - **Suggested command:** `/impeccable harden internal/server/templates/apps.html`

- **[P2] No search, no sort, no count.**
  - **Why it matters:** Fine at 3 apps, hostile at 50. The product brand is "speed and it-just-worked"; at scale the speed dies on a list scroll. Returning users with many apps lose the ability to find quickly.
  - **Fix:** above the `<ul>`, add a thin filter input (`input input-sm`, label "Filter apps") and a `<select>` for sort (Edited recently / Created / A→Z). Add a count next to the H1 ("Your apps · 12"). Render the filter/sort only when `len(.Apps) > 8` so small accounts don't carry the chrome.
  - **Suggested command:** `/impeccable adapt internal/server/templates/apps.html`

- **[P2] Empty state has no mascot.**
  - **Why it matters:** first-time arrival is the brand-promise moment. PRODUCT.md commits "the mascot does the smiling." The empty card is silent — the page where the user's pride will live deserves a single beat of warmth before there's anything to be proud of.
  - **Fix:** inline a 48px banana mascot SVG above the H2 inside the dashed card. Keep the chrome around it sober (no glow, no animation by default). Do NOT extend to the populated state — chrome stays sober there.
  - **Suggested command:** `/impeccable delight internal/server/templates/apps.html`

- **[P3] `⋮` Unicode glyph and `z-20` arbitrary value.**
  - **Why it matters:** PRODUCT.md feedback memory and DESIGN.md both require hand-authored SVG over emoji/glyph icons. The `⋮` renders inconsistently across platforms. `has-[.dropdown:focus-within]:z-20` (line 30) is exactly the magic-number depth the new Named Depth Rule prohibits — `--z-dropdown` exists.
  - **Fix:** replace `⋮` with `<svg viewBox="0 0 16 16" aria-hidden="true" class="size-4 fill-current"><circle cx="8" cy="3" r="1.5"/><circle cx="8" cy="8" r="1.5"/><circle cx="8" cy="13" r="1.5"/></svg>`. Replace `has-[.dropdown:focus-within]:z-20` with `has-[.dropdown:focus-within]:z-dropdown` (and surface the utility if it doesn't already exist).
  - **Suggested command:** `/impeccable distill internal/server/templates/apps.html`

#### Persona Red Flags

- **Sam (a11y).** The `<li>` holds a block-level anchor with an `absolute`-positioned `dropdown` sibling. Screen readers read the entire row text as the anchor name (H2 + timestamp + description + slug + domain — long). The `aria-label="Open live site"` is good; the `⋮` button has `aria-label="More actions"` with no app name. Append `aria-label="More actions for {{ .Name }}"` so screen readers announce row context. The `tabindex="0"` on the `<button>` at line 47 is redundant (buttons are tab-stops by default).
- **Casey (mobile).** `pr-32` (128px) reserved on the row link plus an absolutely-positioned overlay means at 360px viewport the H2 has ~190px before the title hits the overlay zone. A 40-character title clips. The edited timestamp wraps via `flex-wrap` into a second line, but the absolute overlay anchors to the row, not the header line, so it can overlap the description row on long content. Either collapse the overlay to a single `⋮` on mobile (move Open ↗ inside the dropdown) or make the row use a `min-w-0` flex layout instead of absolute overlay.
- **Riley (50 apps + long custom domains).** No `truncate` on the mono footer (lines 39-42). A long domain like `events-2026.club.example.org` plus slug plus `·` separator overflows `text-xs` on mobile. Long titles overflow into the overlay zone if the title is on its own line. No defensive truncation anywhere.

#### Minor Observations

- **Line 30:** `has-[.dropdown:focus-within]:z-20` — Named Depth violation.
- **Line 31:** `block p-4 pr-32` — `pr-32` (128px) is a brittle reserved margin. Either reduce or refactor to flex layout.
- **Line 34:** `<h2 class="text-base font-semibold">` — `text-base` for a row H2 is fine but DESIGN.md's title size is `text-lg`. Worth deciding if list rows are body-rank or title-rank for consistency with manage.html section H2s.
- **Line 36:** "Edited {{ .LastEdited }}" — does the partial pre-format relative time? If raw timestamp leaks through, "Edited 2026-06-04T13:22:01Z" reads cold. Should be "Edited 3m ago" / "Edited 2 days ago" / etc.
- **Line 47:** `<button tabindex="0">` is redundant.
- **Line 67-73:** empty-state H2 is `text-xl`; populated rows use `text-base`. Defensible (single-piece-of-content vs row), but worth being deliberate about.
- **No `aria-current`** on whatever the most recently edited app is.

#### Questions to Consider

1. **Should the list row *be* a card, or should it be a table?** Four affordances per row plus the filter/sort/count needs is what tables solve. A `tabular-nums` row with title / slug / edited / actions columns would scale to 50 apps gracefully and recover the visual budget the current card spending on borders and padding.
2. **What if the empty state shipped the prompt form itself?** Instead of a dashed card pointing to `/`, render the build form here. The non-coder lands on "Your apps," sees zero apps, sees the textarea, types — and the next time they land here, the list. One less click on the most pride-loaded moment.
3. **Where's the "currently building" row?** This page is the destination from a finishing build, but there's no representation of in-progress sites. A `building` row state — animated banana mascot, steps-strip miniature, live SSE — would tie this page to the speed promise.
