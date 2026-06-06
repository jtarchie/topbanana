---
name: Top Banana
description: A vibe-coding hosting platform where a prompt becomes a hosted static site in a minute.
colors:
  primary: "oklch(58.92% 0.199 134.6)"
  primary-content: "oklch(11.784% 0.039 134.6)"
  secondary: "oklch(77.75% 0.196 111.09)"
  secondary-content: "oklch(15.55% 0.039 111.09)"
  accent: "oklch(85.39% 0.201 100.73)"
  accent-content: "oklch(17.078% 0.04 100.73)"
  neutral: "oklch(30.98% 0.075 108.6)"
  neutral-content: "oklch(86.196% 0.015 108.6)"
  base-100: "oklch(98.71% 0.02 123.72)"
  base-200: "oklch(91.8% 0.018 123.72)"
  base-300: "oklch(84.89% 0.017 123.72)"
  base-content: "oklch(19.742% 0.004 123.72)"
  info: "oklch(86.19% 0.047 224.14)"
  success: "oklch(86.19% 0.047 157.85)"
  warning: "oklch(86.19% 0.047 102.15)"
  error: "oklch(86.19% 0.047 25.85)"
  mascot-yellow: "#ffd33a"
  mascot-brown: "#5a3a18"
  mascot-stem: "#7a5a2a"
  mascot-tongue: "#ff8a8a"
  mascot-cheek: "#ff9aa2"
typography:
  display:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "1.875rem"
    fontWeight: 600
    lineHeight: 1.1
    letterSpacing: "-0.025em"
  headline:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "1.5rem"
    fontWeight: 600
    lineHeight: 1.2
    letterSpacing: "-0.025em"
  title:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "1.125rem"
    fontWeight: 600
    lineHeight: 1.3
    letterSpacing: "normal"
  body:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.6
    letterSpacing: "normal"
  label:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "0.875rem"
    fontWeight: 500
    lineHeight: 1.4
    letterSpacing: "normal"
  caption:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 400
    lineHeight: 1.4
    letterSpacing: "normal"
  eyebrow:
    fontFamily: "system-ui, -apple-system, \"Segoe UI\", \"Helvetica Neue\", Arial, sans-serif"
    fontSize: "0.75rem"
    fontWeight: 600
    lineHeight: 1.4
    letterSpacing: "0.05em"
  mono:
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace"
    fontSize: "0.875rem"
    fontWeight: 400
    lineHeight: 1.5
    letterSpacing: "normal"
rounded:
  field: "0.5rem"
  box: "1rem"
  selector: "1rem"
  pill: "9999px"
spacing:
  xs: "4px"
  sm: "8px"
  md: "16px"
  lg: "24px"
  xl: "32px"
  "2xl": "40px"
components:
  button-primary:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.primary-content}"
    typography: "{typography.label}"
    rounded: "{rounded.field}"
    padding: "0 16px"
    height: "2.5rem"
  button-primary-hover:
    backgroundColor: "{colors.primary}"
    textColor: "{colors.primary-content}"
  button-ghost:
    backgroundColor: "transparent"
    textColor: "{colors.base-content}"
    typography: "{typography.label}"
    rounded: "{rounded.field}"
    padding: "0 12px"
    height: "2.5rem"
  button-ghost-hover:
    backgroundColor: "{colors.base-200}"
    textColor: "{colors.base-content}"
  card:
    backgroundColor: "{colors.base-100}"
    textColor: "{colors.base-content}"
    rounded: "{rounded.box}"
    padding: "24px"
  input:
    backgroundColor: "{colors.base-100}"
    textColor: "{colors.base-content}"
    typography: "{typography.body}"
    rounded: "{rounded.field}"
    padding: "0 12px"
    height: "2.5rem"
  textarea:
    backgroundColor: "{colors.base-100}"
    textColor: "{colors.base-content}"
    typography: "{typography.body}"
    rounded: "{rounded.field}"
    padding: "12px"
  alert-success:
    backgroundColor: "{colors.success}"
    textColor: "{colors.base-content}"
    rounded: "{rounded.box}"
    padding: "12px 16px"
  alert-warning:
    backgroundColor: "{colors.warning}"
    textColor: "{colors.base-content}"
    rounded: "{rounded.box}"
    padding: "12px 16px"
  alert-error:
    backgroundColor: "{colors.error}"
    textColor: "{colors.base-content}"
    rounded: "{rounded.box}"
    padding: "12px 16px"
  toggle:
    backgroundColor: "{colors.base-300}"
    rounded: "{rounded.pill}"
    height: "1.5rem"
    width: "2.75rem"
  toggle-checked:
    backgroundColor: "{colors.primary}"
---

# Design System: Top Banana

## 1. Overview

**Creative North Star: "The Lemonade Stand"**

A friendly small-business shop where real work happens at the counter. The painted sign out front is a smiling cartoon banana. Behind it, the kitchen is industrial: prompt goes in, hosted static site comes out, in a minute. The chrome is workshop tooling tinted lemonade-yellow, not toyland fizz. The mascot carries the personality so the layout, type, and hierarchy can stay precise and grown-up. The pleasure for the user is "this is going to be fun, and it's actually going to work."

The system is built on daisyUI 5 (lemonade light, cyberpunk as a high-contrast secondary), self-hosted Tailwind v4, and Inter as the only typeface. Surfaces are flat by default (`--depth: 0`): no shadows, no fuzzy gradients, no glassmorphism. Depth comes from hairline borders, careful spacing, and the `base-100 → base-200 → base-300` lightness ladder. Color carries warmth; shape carries structure. The interface is one yellow accent away from a serious admin panel, and that's the point.

What this system explicitly rejects, pulled forward from PRODUCT.md: generic SaaS dashboard chrome (gradient hero plus identical card grid); childish or cluttered toy aesthetics (round-everything, comic-sans-adjacent noise); stiff enterprise admin (cold gray, twelve tabs deep); and crypto / web3 dark-neon (glow, holographic gradients, "$BANANA" energy). The cyberpunk daisyUI palette ships as a high-energy second theme, not as a brand statement. The brand is not neon.

**Key Characteristics:**
- One typeface (Inter) carries display, body, labels, and microcopy.
- One mascot (the cartoon banana) carries all the whimsy. The chrome stays sober.
- Flat surfaces, hairline `base-300` borders, real `base-200`/`base-300` lightness layering. No shadows.
- Yellow-green primary used for actions and active state only — never for decoration or large fills.
- Progressive disclosure (`<details>`, sub-nav, sidebars) over dashboard density.
- Mobile usability mandatory on the build form; the rest may compact gracefully on small screens.

## 2. Colors: The Lemonade Palette

The palette is a stand of warm light olives, citrus greens, and lemon yellows on a near-white tinted-yellow surface. It reads as a Saturday-morning workshop, not as a "wellness brand." The four `mascot-*` swatches live only inside the banana SVG; do not pull them into the UI.

### Primary
- **Lemonade Pulp** (`oklch(58.92% 0.199 134.6)`): the verb-color. Used on the primary action button, the active state of radio/check controls, the filled `step-primary` build indicator, the `link link-primary` text accent, and `toggle-primary` on. The single most reserved color in the system; if it shows up in a place that isn't an action or a current-state mark, it's misused.
- **Pulp Ink** (`oklch(11.784% 0.039 134.6)`): primary-content. Sits on top of Lemonade Pulp at AA contrast.

### Secondary
- **Pulp Mustard** (`oklch(77.75% 0.196 111.09)`): the second-action / import-form accent. Currently surfaces only on `btn-secondary` (Import site form on the landing page).

### Tertiary (Accent)
- **Sunlit Lemon** (`oklch(85.39% 0.201 100.73)`): a high-lightness yellow held in reserve for accent-only use (`btn-accent`, future highlight states). Not used in the current first-party UI; documented because daisyUI exposes it.

### Neutral
- **Olive Earth** (`oklch(30.98% 0.075 108.6)`): daisyUI `neutral` role for darker UI chrome (currently unused in admin templates; reserved for future toolbars / dark panels).
- **Lemonade Sky** (`oklch(98.71% 0.02 123.72)`): `base-100`, the canvas. A near-white with a 2-chroma yellow tint. The `bg-base-100` of every page body, card, dropdown, and panel.
- **Pith** (`oklch(91.8% 0.018 123.72)`): `base-200`, the second surface layer. Footer background, inline `<code>` pill background, table-zebra alt rows.
- **Rind** (`oklch(84.89% 0.017 123.72)`): `base-300`, the hairline border value used on every card, input, table, and divider. Carries the structure that the missing shadows don't.
- **Banana Bark** (`oklch(19.742% 0.004 123.72)`): `base-content`, the ink. Body text, headlines, icon strokes. Read at 70% opacity (`text-base-content/70`) for muted copy, 60% (`/60`) for captions, 30%/40% for separators only.

### Semantic
The four daisyUI semantic colors (info, success, warning, error) all sit at lightness 86.19% in lemonade. They are pastel by default; on alerts they render as a tinted surface, not a saturated fill.
- **Info** (`oklch(86.19% 0.047 224.14)`): used as `bg-info/10 border-info/30` on the workspace status strip; never full-saturation.
- **Success** (`oklch(86.19% 0.047 157.85)`): `alert-success` for flash confirmations.
- **Warning** (`oklch(86.19% 0.047 102.15)`): `alert-warning` for quota / soft limits.
- **Error** (`oklch(86.19% 0.047 25.85)`): `alert-error` and `text-error` (delete labels, validation copy). The `btn-error` action is reserved for confirmed-destructive operations only.

### Named Rules

**The One Yellow Rule.** Lemonade Pulp is used on ≤10% of any given screen. Its rarity is the reason it reads as the verb. Decoration in Lemonade Pulp is forbidden: no large background fills, no gradient backgrounds, no Pulp-on-Pulp.

**The Hairline Rule.** Where another system would reach for `box-shadow`, this one reaches for `border border-base-300`. Cards, inputs, dropdowns, alert containers, status strips: 1px Rind borders carry the layering. The `--depth: 0` in the daisyUI theme is the doctrine, not a default to override.

**The 70 / 60 Rule.** Muted body copy sits at `text-base-content/70`; captions, slug pills, timestamps at `/60`. Never lower than 60% without raising the type size to 16px or hitting AA explicitly. The single biggest contrast risk in the system is reaching for `/50` "for elegance" on small print.

**The Dark-Mode Rule.** `lemonade-dark` is the brand dark mode — same Lemonade Pulp primary, the base ladder inverted (dark Banana-Bark surface, light Lemonade-Sky ink), still flat, still hairline. The theme toggle swaps `lemonade` ↔ `lemonade-dark`; `prefers-color-scheme: dark` lands on `lemonade-dark`. The `cyberpunk` daisyUI theme (hot pink on bright yellow, hard-cornered) remains reachable from Theme Studio but is not the auto dark target — PRODUCT.md's anti-references reject crypto/web3 dark-neon. New themes added to Theme Studio must keep that boundary explicit.

## 3. Typography

**Display Font:** System sans (`system-ui, -apple-system, "Segoe UI", "Helvetica Neue", Arial, sans-serif`)
**Body Font:** System sans (same stack, weights 400 / 500 / 600 / 700)
**Mono Font:** `ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace` (system-resolved)

**Character:** the system sans stack resolves to SF Pro on macOS / iOS, Segoe UI on Windows, Roboto on Android, and the platform default elsewhere. One platform-native family does the entire system; hierarchy is carried by weight contrast (semibold on headings, medium on labels, regular on body) and a tight rem scale, not by a second display family. No webfont download, no CDN dependency, no FOIT — matches the project's self-hosted-everything stance. Letter-spacing is tightened to `tracking-tight` on H1 and H2 only; everything else sits at `normal`. Italic, oblique, and small-caps are out of scope. Monospace exists for slugs, domains, paths, and inline code — not for stylistic effect.

### Hierarchy
- **Display** (semibold 600, `text-3xl sm:text-4xl` ≈ 30–36px, line-height 1.1, `tracking-tight`): only on the landing `Build a new app` H1. Used once per page maximum.
- **Headline** (semibold 600, `text-2xl sm:text-3xl` ≈ 24–30px, line-height 1.2, `tracking-tight`): every other page H1 (Your apps, Manage, Account, System, Workspace empty state). The default page entry.
- **Title** (semibold 600, `text-lg` ≈ 18px or `card-title text-lg`): section H2 inside cards (General, Permissions, Form submissions).
- **Body** (regular 400, `text-base` ≈ 16px on prose / `text-sm` ≈ 14px in dense UI, line-height 1.6 prose / 1.5 UI): paragraph copy. Prose surfaces (landing description, manage card descriptions) cap line length at 65–75ch on desktop via `max-w-3xl` containers. Tables are exempt and may run denser.
- **Label** (medium 500, `text-sm` ≈ 14px, line-height 1.4): form labels (`.label > span.text-sm.font-medium`), button text inside `.btn`, list-item headings on cards.
- **Caption** (regular 400, `text-xs` ≈ 12px, `text-base-content/60`): timestamps, slug pills, supporting microcopy ("Edited 3m ago", "Lowercase letters, digits, and hyphens").
- **Eyebrow** (semibold 600, `text-xs` ≈ 12px, `uppercase tracking-wide`, `text-base-content/60`): reserved for the workspace sidebar group headings (Pages / Images / Tools / Server functions) and **only those**. Not a section-decoration pattern. See the rule below.
- **Mono** (regular 400, `text-xs` or `text-sm`, system mono): slugs, custom domains, file paths, inline `<code>` (`bg-base-200 px-1 rounded font-mono`).

### Named Rules

**The Eyebrow Rule.** Tiny uppercase tracked text is a sidebar grouping device, not a section accessory. It appears in `aside` group headings (Pages, Images, Tools, Server functions). Putting an uppercase eyebrow above every page section is the saturated AI scaffold and is prohibited. If a section needs introduction, it gets a real H2 in title weight at title size.

**The One Family Rule.** The system sans stack carries every type role except mono. A webfont, a display serif, a geometric sans — anything that adds a network request or a second family — is prohibited without explicit brand sign-off. The mono fallback chain is also system-resolved; do not introduce a custom mono webfont.

**The Tight Scale Rule.** The ratio between adjacent type steps is roughly 1.2× (14 → 16 → 18 → 24 → 30 → 36). The system reaches for weight contrast (400 → 500 → 600) before scale jumps. There is no clamp() heading scale; this is a product UI viewed at consistent DPI, and fluid headings make sidebar panels worse, not better.

## 4. Elevation

The system is flat. `--depth: 0` is set in the daisyUI lemonade theme on purpose. Surfaces stack via the `base-100 → base-200 → base-300` lightness ladder and 1px Rind borders, never via shadow.

The two exceptions, both narrow:
1. The apps-list dropdown menu (`.dropdown-content`) carries daisyUI's default `shadow` token because it has to read as floating over the row underneath. This is the only stationary `shadow` use in the admin UI.
2. The native `<dialog>` confirm modal renders the browser's default `::backdrop` overlay (`rgba(0,0,0,.3)` via the `side-scrim` analog). This is system chrome, not authored elevation.

### Named Rules

**The Flat Rule.** New components default to `border border-base-300` for separation. Reach for shadow only when the element is genuinely floating over content the user is mid-task in (popovers, dropdowns, side panels with backdrops). Decorative shadows are prohibited.

**The Ladder Rule.** When two surfaces are adjacent and you need to distinguish them, lighten one step on the base ladder (Rind → Pith → Lemonade Sky) before introducing a border or a shadow. Footer-on-page, table-zebra rows, sidebar-on-content all use this lever.

## 5. Components

For every interactive component the system already ships: default, hover, focus, active. Disabled and loading where they apply. Half-state component vocabulary is prohibited.

### Buttons
- **Shape:** `0.5rem` corner radius on every button regardless of variant. `2.5rem` minimum height enforced in `app.input.css` (`.btn { min-height: 2.5rem; }`).
- **Primary** (`btn btn-primary`): Lemonade Pulp background, Pulp Ink text, medium-weight label. Used on the single most important action per surface (Create site, Save changes, New app). Hover deepens primary by 5% lightness, transitions in 200ms.
- **Ghost** (`btn btn-ghost`): transparent background, Banana Bark text. The default for secondary actions: header navigation, dropdown triggers, sidebar tools, the "Open ↗" affordance on app rows. Hover fills with Pith (`base-200`).
- **Secondary** (`btn btn-secondary`): Pulp Mustard background. Reserved for parallel-but-not-primary submission actions (Import site form). Do not introduce a third "secondary-but-different" variant; if you need three actions, demote two of them to ghost.
- **Error** (`btn btn-error`): error tint background, used on destructive confirms (Delete inside `confirm_dialog`). Never used on standalone navigation.
- **Sizing:** `btn-sm` (2rem height) for header chrome, dropdown items, and table-row actions. `btn-square` for icon-only triggers (theme toggle, the `⋮` more-actions menu).
- **Focus:** every button picks up the global `:focus-visible { outline: 2px solid var(--color-primary); outline-offset: 2px; }` ring. Do not suppress it per-button.

### Cards / Containers
- **Corner Style:** `rounded-box` = 1rem on every card; nothing smaller, nothing larger.
- **Background:** Lemonade Sky (`bg-base-100`). Never tinted, never gradient.
- **Border:** 1px Rind (`border border-base-300`) at all times. Hover may shift to `border-base-content/30` for clickable rows (apps list).
- **Shadow Strategy:** none. See Elevation.
- **Internal Padding:** `card-body` ships at 1.5rem (24px) by default; the landing form's outer card uses the same. Inline lists and form sections inside cards use `space-y-8` between groups, `gap-6` inside groups.
- **Dashed variant:** empty states use `border-dashed` on the same `border-base-300` to read as "no content yet" rather than "broken container" (apps list empty state).

### Inputs / Fields
- **Style:** `bg-base-100` with `border border-base-300`, `rounded-field` (0.5rem), 2.5rem height (`input`, `select`) or natural height (`textarea`). The daisyUI `.input` and `.textarea` classes carry these defaults; do not override.
- **Focus:** border shifts to Lemonade Pulp; the global focus-visible ring augments it. No glow, no shadow.
- **Placeholder:** must read at ≥4.5:1. daisyUI's default placeholder is fine on Lemonade Sky but must be re-verified on Pith before reuse.
- **Mono fields:** `font-mono text-sm` for slugs, custom domains, file paths (manage.html domains textarea, workspace rename input).
- **File inputs:** daisyUI `.file-input` styled to match the rest of the form. Used in landing.html for the reference-docs upload.

### Radio / Check / Toggle
- **Radio cards** (landing template picker): full label is the hit target. Selected card border shifts to Lemonade Pulp and gains `ring-1 ring-primary` via `has-[:checked]` selectors. Radio itself stays daisyUI default (`radio radio-primary radio-sm`).
- **Toggles** (manage permissions): daisyUI `.toggle.toggle-primary`. On-state fills with Lemonade Pulp; off-state is `base-300`. The clickable region is the full row, not just the toggle.

### Alerts
- **Style:** pastel semantic tint as background (`alert-success` / `alert-warning` / `alert-error` / `alert-info`), Banana Bark text, `role="status"`. No icon stacking by default; icons are optional, not load-bearing.
- **Placement:** above the content they relate to (flash messages above the page H1, quota warnings above the apps list, lint flash above the workspace canvas).

### Navigation
- **Top bar** (`brand` partial): single horizontal `.navbar` with `bg-base-100`, `border-b border-base-300`, mascot SVG + wordmark on the left, ghost-button nav links on the right (Your apps / Users / System / Account), theme toggle at the far right.
- **Site subnav** (`site_subnav` partial): daisyUI `.tabs.tabs-bordered` under the top bar when scoped to a single site. Workspace / Manage tabs, plus a far-right "View site →" link.
- **Active state:** `btn-active` on top-bar ghosts; `tab-active` on subnav tabs. Both use Banana Bark text on a slightly Pith fill — no Lemonade Pulp on inactive nav items.
- **Mobile:** navbar wraps; subnav scrolls horizontally. There is no hamburger sidebar; the surface is shallow enough.

### Side panel (signature)
- **Pattern:** fixed-position right-anchored panel (`width: min(420px, 100vw)`) with a `data-open="true"` data-attribute toggle. Slides in via `transform: translateX(0)` over 200ms ease. Backdrop (`.side-scrim`) is `rgba(0,0,0,.3)` and shares the same toggle.
- **Triggers:** any element with `data-panel="<name>"` opens panel `id="panel-<name>"`. Escape closes every open panel.
- **Use:** Themes, Version history, and future tool panels inside the workspace. Not used for forms; not used for nav.

### Confirm dialog (signature)
- **Pattern:** native `<dialog>` element with daisyUI `.modal-box`, opened via `dialog.showModal()` when a `.js-confirm` form submits. Hooks into per-form `data-confirm-title` / `data-confirm` / `data-confirm-ok` / `data-confirm-tone` attributes.
- **Style:** Lemonade Sky background, Banana Bark text, Rind hairline. Confirm action is `btn btn-error` for destructive ops, `btn btn-primary` when `data-confirm-tone="primary"`.

### Steps (build status)
- **Pattern:** daisyUI `.steps.steps-horizontal` strip inside a tinted info status bar (`bg-info/10 border-b border-info/30`). Each step (`.step`) gains `.step-primary` as the build advances through Starting / Designing / Polishing / Ready.
- **Loading:** the daisyUI `.loading.loading-spinner` sits left of the message, sized `loading-sm`, color `text-primary`. This is the only spinner the system ships; never inline-replace content with a spinner — use the steps strip.

### Banana mascot (signature)
- **Source of truth:** hand-authored SVG at `internal/server/favicon.svg`. Same artwork is inlined verbatim into the `brand` header partial.
- **Palette:** `#ffd33a` body, `#5a3a18` outline, `#7a5a2a` stem, `#5a2a2a` mouth interior, `#ff8a8a` tongue, `#ff9aa2` cheeks (at 70% opacity), `#fff`/`#1a1a1a` eyes. These hex values appear only inside the banana SVG and are not reused elsewhere in the system.
- **Sizing:** 24px in the header. 16-32px in any other admin use. Do not scale the mascot above 48px in admin chrome; the brand promise is product-first, not character-first.
- **Modifications:** no facial swaps, no hand/arm additions (a previous thumbs-up variant read as a rude gesture at favicon size and was removed), no rotations, no color shifts. If the surface needs a different mood, use a different SVG and reserve this one for "Top Banana itself."

## 6. Do's and Don'ts

### Do:
- **Do** reach for `border border-base-300` (Rind) when separating two surfaces. The Hairline Rule is the system's depth strategy.
- **Do** keep Lemonade Pulp at ≤10% of any screen. Primary buttons, active radios, current tab, link accents. Nothing else.
- **Do** use the `base-100 → base-200 → base-300` ladder for adjacent surfaces (footer-on-page, sidebar-on-content, zebra-row-on-row) before reaching for a border or shadow.
- **Do** lead with progressive disclosure (`<details>`, sub-nav, side panels) on the build form, manage, and account surfaces. The first-time user should see one button.
- **Do** carry the global `:focus-visible` ring on every interactive element. The ring is 2px Lemonade Pulp with 2px offset; suppressing it per-element is a regression.
- **Do** keep muted copy at `text-base-content/70` and captions at `/60`. Re-verify both against `base-100` and `base-200` whenever a new surface is introduced.
- **Do** size body type by a tight rem scale (14 / 16 / 18 / 24 / 30 / 36). Weight contrast (400 → 500 → 600) is the first lever; scale is the second.
- **Do** keep the banana mascot at its source-of-truth SVG, at 16–32px, in the lemonade colors documented above. The chrome stays sober; the banana does the smiling.
- **Do** honor `prefers-reduced-motion: reduce` on the side-panel slide, the build-stream steps, and any future motion.

### Don't:
- **Don't** add gradients of any kind. No gradient hero, no `background-clip: text` on headings, no gradient borders, no gradient backgrounds on cards. Top Banana does not own that visual.
- **Don't** ship glassmorphism (`backdrop-filter`, frosted cards, blurs on chrome). This is on PRODUCT.md's anti-reference list verbatim ("no fuzzy gradients, no glassmorphism, no cream-tinted 'warm' near-whites used as a default surface").
- **Don't** use `border-left` or `border-right` greater than 1px as a colored accent. Side-stripe borders are an absolute ban.
- **Don't** put a tiny uppercase tracked eyebrow above every section. Eyebrows are reserved for the workspace sidebar group headings (Pages / Images / Tools / Server functions) and nothing else.
- **Don't** use numbered scaffolding ("01 · About / 02 · Process") to dress up sections. Numbers appear only when the section *is* an ordered sequence (the build steps strip).
- **Don't** introduce a webfont or a second typeface. The system sans stack carries display, body, and labels; the mono fallback chain is system-resolved. Adding a webfont reintroduces a network request and contradicts the project's no-CDN stance.
- **Don't** treat the daisyUI `cyberpunk` theme as the brand. `lemonade-dark` is the brand dark mode. Cyberpunk is a high-contrast Theme-Studio option for users who want loud; it is not "Top Banana in dark mode," it is not for marketing surfaces, and the brand's anti-reference list explicitly rejects "Crypto / web3 dark-neon aesthetics" (glow, holographic gradients, "$BANANA" energy).
- **Don't** drop muted copy below `text-base-content/60` for body or label sizes. The single biggest contrast regression in this system is reaching for `/50` "for elegance" on small print.
- **Don't** ship Lemonade Pulp as a large fill (hero banner, full-bleed CTA strip, drenched section). The One Yellow Rule applies — it is a verb-color, not a section-color.
- **Don't** add a hero-metric panel (big number + small label + supporting stats + gradient accent). It's a SaaS cliché on PRODUCT.md's anti-reference list.
- **Don't** reinvent standard affordances (custom scrollbars, weird modals, non-standard form controls). daisyUI's defaults are the system; if a new pattern is genuinely needed, document it here first.
- **Don't** modify the banana mascot SVG to add hands, thumbs, weapons, or props. A previous thumbs-up variant read as a rude gesture at favicon size and was removed for a reason.
