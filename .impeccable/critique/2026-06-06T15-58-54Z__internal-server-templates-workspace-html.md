---
target: internal/server/templates/workspace.html
total_score: 24
p0_count: 1
p1_count: 2
timestamp: 2026-06-06T15-58-54Z
slug: internal-server-templates-workspace-html
---
#### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Build strip + steps + flash + toast is real, but `es.onerror = function () { /* auto-reconnect */ };` is silent: a dropped SSE shows no indicator, the spinner spins forever |
| 2 | Match System / Real World | 3 | "Re-run checks" is jargon for non-coders (vs "Validate site"); the post-build toast for image upload says "Image added." not "Saved." |
| 3 | User Control and Freedom | 3 | Version history is excellent; textarea content isn't preserved across submit reload; no way to abort a running build |
| 4 | Consistency and Standards | 2 | Three button sizes for "primary action" weight (sidebar `btn-sm`, theme panel `btn-xs`, history `btn-xs`); three close-affordances (`×` text, `×` text, dialog `close`); no icon vocabulary |
| 5 | Error Prevention | 3 | File delete / Restore / Delete version all use `js-confirm` and read well; rename submits on Enter with no confirm or validation though it can break inbound links |
| 6 | Recognition Rather Than Recall | 3 | Selection chip showing picked text is recognition done right; sidebar Tools buttons carry no state indicator for staged-but-not-applied theme |
| 7 | Flexibility and Efficiency | 2 | No keyboard shortcuts (no T/H to open panels, no Cmd-Enter to submit); tab order is page → sidebar → form, not prompt-first |
| 8 | Aesthetic and Minimalist Design | 2 | Density fine; primary action has no visual rank, glassmorphism in preview overlay, no H1 on the page |
| 9 | Error Recovery | 2 | Build failure renders raw `ev.message` with no retry, no "see logs"; upload errors toast and vanish in 4.5s; theme apply failure loses context |
| 10 | Help and Documentation | 1 | Zero inline help. No "What is a server function?" tooltip; the clarify card's "Our suggestion" is the closest thing to guidance |
| **Total** | | **24/40** | **Acceptable — significant improvements before users are happy** |

#### Anti-Patterns Verdict

**Start here. Does this look AI-generated?**

**LLM assessment:** Register is mostly right. Sober workshop chrome, no gradient hero, no hero-metric scaffolding, no numbered "01 · / 02 ·" decoration. The page reads as a tool, not a brochure. Specific tells worth naming:

- **The preview overlay is a literal glassmorphism violation.** Line 117: `class="bg-base-100/80 backdrop-blur-sm px-4 py-2 rounded-box border border-base-300"` is the exact "frosted card over content" pattern DESIGN.md's Do's-and-Don'ts bans by name. The justification ("user can vaguely see the old site through it") is the same justification glass always uses. Hairline Rule has a perfectly good answer: opaque `bg-base-100`, hairline border, drop the blur.
- **The Eyebrow Rule pushed close to its limit.** Pages / Images / Tools / Server functions in the sidebar (4 eyebrows) is the documented exception. The JS-built theme-gallery `<h3>` per category (`text-xs uppercase tracking-wide`, line 318) is a *fifth* eyebrow pattern, and the panel body isn't really a sidebar. On-rule by loosest read; off-rule by spirit.
- **z-index is unnamed and ad-hoc.** `z-40` scrim, `z-50` panel, `z-[60]` toast. The arbitrary Tailwind escape hatch for the toast is exactly the magic-number depth DESIGN.md prohibits.
- **`shadow-xl` on side panels is the heaviest stock shadow.** Side panels are an allowed Hairline-exception, but `shadow-xl` is daisyUI's sledgehammer (24-25px blur). The Flat Rule says "reach gently"; `shadow-lg` or `-md` carries the floating semantic without screaming.
- **Missed brand opportunity:** the post-build "Your site is ready." moment is just a status-msg text swap. No mascot beat, no toast, no peak. The brand is "friendly, cheeky, capable" — capable and sober is on the page; the cheek is entirely absent.

**Deterministic scan:** `detect.mjs --json internal/server/templates/workspace.html` exited 2 with one finding:

```json
[{
  "antipattern": "flat-type-hierarchy",
  "name": "Flat type hierarchy",
  "severity": "warning",
  "file": ".../workspace.html",
  "line": 15,
  "snippet": "Sizes: 12px, 14px, 16px (ratio 1.3:1)"
}]
```

The detector's `1.3:1` is the span across the full size set (16/12 = 1.33), not the adjacent step ratios — `14/12 = 1.17` and `16/14 = 1.14` both sit below the 1.25 floor. The detector is right about the *substance* even if the displayed ratio is misleading. Read in the light of Assessment A: there is no H1, no display tier, no large heading anywhere on this page. The eye has nothing above `text-base` to anchor it. The detector + LLM converge on the same root cause: visual rank is missing.

**Visual overlays:** unavailable. Dev server isn't running; the file is a Go template, so a static load would render `{{ ... }}` as text. No live overlay was injected.

#### Overall Impression

This is a real, dense tool surface — and it under-uses its own design system. The components are correct (cards are hairlined, base ladder is layered, daisyUI primitives clean). What's missing is **rank.** There is no H1, the primary action shares its size class with every secondary tool button, the post-build celebration is silent, and the preview overlay is the one place the Hairline Rule is broken. Fix rank — type, action, moment — and the page lifts from "competent admin" to "workshop you want to be in."

The single biggest hidden opportunity: the post-build moment. "Your site is ready" lands as a flat string. PRODUCT.md says the brand promise is speed plus delight; the delight budget has been entirely spent on the header banana. This page is where the wink should pay off.

#### What's Working

1. **The clarify-question card** (`renderQuestionCard`, lines 483+). "Use this suggestion" primary button + alternative options + "Or tell us in your own words…" fallback is human, not robotic. Honors progressive disclosure; reads like someone wrote it for Mira, not Alex.
2. **Version history copy** (line 190): *"A new version is saved before every change. Restore brings the site back to that state — and saves the current state first, so you can always come forward."* Concrete, present-tense, removes fear of clicking. This is what every other piece of microcopy on the page should sound like.
3. **The selection chip** bridging postMessage from the iframe into the prompt as `On: "<excerpt>"`. Quietly magical. Recognition over recall; the user knows exactly what their next sentence will apply to.

#### Priority Issues

- **[P0] Primary action has no visual rank.**
  - **Why it matters:** "Apply change" is `btn btn-primary btn-sm` — same size as Themes, Version history, Visual editor, Re-run checks, Export site. Lemonade Pulp is the only differentiator. On a small screen or for a Pulp-color-blind user, nothing screams "this is the verb." DESIGN.md's Tight Scale Rule says weight contrast first, scale second — this page took *neither* lever. There's also no H1, no display tier, no anchor above `text-base`.
  - **Fix:** drop `btn-sm` on the submit (`#edit-form button[type=submit]`, line 150) to use the default 2.5rem; keep the sidebar buttons at `btn-sm` so the contrast lands. Add a true page H1 (`text-xl font-semibold`, visually for sighted users; could be sr-only at minimum) carrying the page name `Workspace · {{ .SiteName }}`. Set the page's auto-focus to `#prompt` so the editor opens with the cursor in the composer.
  - **Suggested command:** `/impeccable polish internal/server/templates/workspace.html`

- **[P1] Glassmorphism in the preview overlay.**
  - **Why it matters:** line 117's `bg-base-100/80 backdrop-blur-sm` violates a named ban. It's the one place on the page where the system's discipline drops. The iframe behind is already `opacity-30`; the pill is overdesigning a problem that doesn't exist.
  - **Fix:** swap to `bg-base-100 px-4 py-2 rounded-box border border-base-300`; drop `backdrop-blur-sm`. Per Hairline Rule.
  - **Suggested command:** `/impeccable quieter internal/server/templates/workspace.html`

- **[P1] The "Your site is ready" moment is flat.**
  - **Why it matters:** the brand promise lands here. Currently: 600ms after `ev.status === 'completed'`, the strip vanishes, opacity removes from the iframe, no toast, no mascot beat. The peak of the session is empty. "Capable" landed; "friendly + cheeky" didn't.
  - **Fix:** on `completed`, fire `toast('Your site is ready.', 'success')` *and* briefly scale the brand banana SVG in the navbar (e.g. `transform: scale(1.15)` for 250ms, easing back). Respect `prefers-reduced-motion`. One beat, one second, gone.
  - **Suggested command:** `/impeccable delight internal/server/templates/workspace.html`

- **[P2] SSE silently reconnects; build failures dead-end.**
  - **Why it matters:** `es.onerror = function () { /* auto-reconnect */ };` is genuinely silent — a non-coder watching their first build go wrong has no signal that anything's happening. On `failed`, `ev.message` renders raw in the error strip with no retry path, no "see logs" affordance.
  - **Fix:** on `es.onerror`, set `#status-msg` to "Lost connection. Reconnecting…" and show a Retry button if reconnect fails twice. On `failed`, render a "Try again" button that resubmits the last prompt (cache it in `sessionStorage`). On both, the toast helper exists but isn't wired to use `aria-live`; add `role="status" aria-live="polite"` to `#toast`.
  - **Suggested command:** `/impeccable harden internal/server/templates/workspace.html`

- **[P2] z-index and shadow vocabulary is numeric and ad-hoc.**
  - **Why it matters:** `z-40` (scrim), `z-50` (panel), `z-[60]` (toast) is exactly the magic-number depth DESIGN.md prohibits. `shadow-xl` is the heaviest stock shadow on a system that says "reach gently."
  - **Fix:** add `--z-scrim: 40; --z-panel: 50; --z-toast: 60;` tokens in `app.input.css`, reference them via `var(--z-toast)` etc. Replace `z-[60]` and the `.side-panel { z-index: 50 }` literal with the tokens. Downgrade `shadow-xl` to `shadow-lg` on side panels.
  - **Suggested command:** `/impeccable distill internal/server/templates/workspace.html`

#### Persona Red Flags

- **Alex (power user):** no `Cmd-Enter` to submit the prompt; no `T`/`H` shortcuts to open Themes/History; tab order is sidebar-first not prompt-first; toast vanishes in 4.5s with no "recent toasts" history. The bridged-selection chip is the *only* power-user feature that lands. Friction at every speed beat.
- **Sam (a11y, keyboard + screen reader):** `#toast` has no `aria-live` region — toast messages are visual-only. The selection-chip close button is a raw Unicode multiplication sign (`<button>×</button>`); screen reader reads "times." The theme swatches' applied/previewing state is communicated only by border color + a `.tag` text node ("applied" / "previewing"); no `aria-pressed` or `aria-current`. Build steps `<li>` items don't announce as they advance.
- **Mira (book-club organizer, returning Tuesday after editing Sunday):** lands on the workspace; the only orientation is the navbar slug + the sidebar list. No "Last edited Sunday 3pm" anywhere on the page. She has to remember which page she was working on. The clarify card (when present) is the only place that talks *to* her; the rest of the chrome is a tool.

#### Minor Observations

- **Line 235:** `e.data.type !== '_bh_sel'` — leftover Bloomhollow prefix in the postMessage protocol. The theme postMessage uses `topbanana:settheme`; this one didn't get the rename. Should be `_tb_sel` or `topbanana:selection`.
- **Toast lifetime is 4500ms (line 267)** — too short for screen readers, too short for users with motor delay. Lift to 8000ms and pause on hover.
- **Rename form (line 95) submits on Enter** with no confirm, even though changing a filename can break inbound links. Match the delete pattern: `js-confirm` with a "renaming may break links pointing at the old URL" warning for index.html / linked pages.
- **The `#upload-status` text overwrites silently** with no announcement. Use the toast helper for the success path consistently.
- **The `confirm_dialog` OK button rewrites its className** on every open. Works today; brittle (a missing `data-confirm-tone` would inherit the previous tone). Could anchor on a default reset.
- **The selection chip's `line-clamp-2`** truncates with no expand affordance. A user picking a long paragraph can't see exactly what was bridged.
- **The "Add image" file input is hidden** behind a styled label; the input's accept is image-only. Good. But there's no drag-and-drop hint despite the `id="drop-zone"` wrapper.
- **The "preview will appear once the build finishes" pill** could carry the build's current sub-message instead — when the agent is "Adding contact.html…" that's what the overlay should say, not a static "preview will appear."

#### Questions to Consider

1. **Should the workspace open with a single focused composer instead of a four-pane editor?** The primary user — a non-coder editing — probably wants "what do you want to change?" not a file tree. Pages / Tools / History could collapse to a single "More" drawer until the second visit. Right now the page is shipping the developer's mental model of editing to the non-coder.
2. **What if the "preview while building" state were the celebration, not the apology?** Instead of a frosted pill saying "Preview will appear once the build finishes" — show the prompt being built, sentence by sentence, as the agent thinks. The build is the show; we're hiding it behind 30% opacity and a piece of glass.
3. **Why are Themes and Version history sidebar buttons rather than peer tabs to Workspace / Manage?** They're sticky, persistent, site-scoped state. Promoting them to subnav tabs would free the sidebar to be only Pages + Images + Functions — three things, not four-plus-a-grab-bag.
