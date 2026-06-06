---
target: internal/server/templates/landing.html
total_score: 24
p0_count: 2
p1_count: 2
timestamp: 2026-06-06T13-59-46Z
slug: internal-server-templates-landing-html
---
#### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | No "you'll see it build live" reassurance near the primary button; no character counter on the 4096-cap textarea |
| 2 | Match System / Real World | 3 | "Slug" leaks once in the disclosed placeholder; otherwise plain English |
| 3 | User Control and Freedom | 3 | No draft persistence on refresh; no "start over" affordance |
| 4 | Consistency and Standards | 3 | Import-site slug field has no label, while the main form's slug sits behind a labeled `<details>` |
| 5 | Error Prevention | 2 | Slug pattern + minlength fail silently until POST; file input promises "up to 10 files, 64 KB each" but doesn't enforce client-side |
| 6 | Recognition Rather Than Recall | 3 | 12 templates rendered as 12 identical bordered rectangles; no per-template visual cue |
| 7 | Flexibility and Efficiency | 2 | No Cmd/Ctrl+Enter submit, no keyboard shortcut between templates, no recently-used template |
| 8 | Aesthetic and Minimalist Design | 3 | Sober and on-register; loses a point for visual monotony inside the form card |
| 9 | Error Recovery | 1 | No inline error region; HTML5 native popups are the only fallback; no `aria-describedby` on `#slug` |
| 10 | Help and Documentation | 2 | Help is implicit in placeholders; no "What is a template?" tip, no sample-output peek |
| **Total** | | **24/40** | **Acceptable — significant improvements before users are happy** |

#### Anti-Patterns Verdict

**Start here. Does this look AI-generated?**

**LLM assessment:** Believable as AI-made, but the slop is restraint-flavored, not gradient-flavored. The page commits zero of the absolute bans: no side-stripes, no gradient text, no glassmorphism, no hero-metric, no numbered scaffolding, no decorative eyebrows. That's the good news.

The slop cues are subtler:

- **An identical-card grid by another name.** 12 templates render as 12 visually indistinguishable bordered rectangles differentiated only by 14px label text. The absolute ban on "identical card grids" applies to repeated icon-heading-text tiles; this page replaces icons with radios but the visual rhythm is the same.
- **A textbook SaaS-form composition.** Stacked fields inside a card. Could ship under any SaaS logo if you swapped the H1 and the wordmark. Register-correct, identity-poor.
- **The mascot does not actually do the smiling.** PRODUCT.md commits "the mascot does the smiling, the chrome stays sober." The smiling happens on exactly one square inch of pixels above the fold (the 24px header SVG), then vanishes. The chrome dutifully stays sober; nothing else carries the wink.
- **The brand promise is told, not shown.** The subhead promises a working site in a minute; nothing on the page reinforces it visually (no example output, no example prompt chips, no count of sites built today, no timer chip near the button).

**Deterministic scan:** `node .claude/skills/impeccable/scripts/detect.mjs --json internal/server/templates/landing.html` exited 0 with `[]` — zero findings. The detector and the design review agree: no banned patterns are present on this surface. The detector misses what the design review caught, because the failure mode here is composition and copy presence, not CSS shapes. Reading the two together: this page is technically clean on every catchable rule and emotionally flat on every uncatchable one.

**Visual overlays:** Not available. The dev server (`task local`) is not running, and the file is a Go template — opening it as a static page would render the `{{ ... }}` directives as text. No live overlay was injected.

#### Overall Impression

This is a competent product surface that ducks its own brand promise. The register is correct, the daisyUI primitives are used cleanly, the progressive disclosure on slug and attachments shows real care. But the page nails "it's actually going to work" and ducks "fun." The single biggest opportunity is the template grid — it dominates the visual field with 12 equal-weight choices on a page whose lede is "Describe your site," and it's the first thing a non-coder must sort through before they can act.

The second-biggest opportunity is the dark-mode theme swap. Auto-switching OS-dark-mode users to daisyUI's `cyberpunk` palette (hot pink primary on bright yellow surface) drops them straight into the exact "crypto/web3 dark-neon" register PRODUCT.md and DESIGN.md explicitly reject. The brand promise dies in the first 200ms for any user on a Mac with dark mode on.

#### What's Working

1. **Register discipline.** Single H1, no decorative eyebrow, no numbered scaffolding, no gradient text, no shadows on the form card. The page is genuinely a product surface, not a marketing page in disguise. DESIGN.md's "chrome stays sober" doctrine is honored throughout.
2. **Progressive disclosure on slug and attachments.** Both correctly hidden behind `<details>`; the first-time user sees prompt + templates + button by default, not a 4-section form. This is exactly what PRODUCT.md's "power without intimidation" principle calls for.
3. **`has-[:checked]` radio-card styling.** Full label is the hit target; selection state is a hairline border + 1px ring in Lemonade Pulp, no fill. Respects the One Yellow Rule and uses modern CSS that doesn't need JS.

#### Priority Issues

- **[P0] The template grid is a 12-option cognitive wall.**
  - **Why it matters:** the form's lede is "Describe your site," yet the visually dominant block is 12 identical bordered rectangles. A curious non-coder must read 24 lines of label-and-description before clicking Create site; the "minute" promise is broken before she starts. Decision points above 4 options trigger working-memory overload (Miller / Cowan); 12 is in skip-or-abandon territory.
  - **Fix:** collapse templates behind a `<details>` defaulting to "Blank" (matching the existing checked default), with a "Start from a template instead" affordance — or surface 3 curated suggestions (`blank`, `landing-page`, `event`) prominently and put the other 9 behind a "More templates" disclosure. The container is the `#template-cards` parent block (landing.html lines 26–41).
  - **Suggested command:** `/impeccable distill internal/server/templates/landing.html`

- **[P0] Cyberpunk-on-prefers-dark violates a stated brand anti-reference.**
  - **Why it matters:** `layout.html` lines 11–21 auto-switch to daisyUI's `cyberpunk` theme (hot pink primary, bright yellow surface, hard-cornered) for any user whose OS reports `prefers-color-scheme: dark`. That is exactly the "crypto/web3 dark-neon" register PRODUCT.md anti-references and DESIGN.md's Cyberpunk Caveat reject as "not the brand." A non-coder on a Mac in dark mode lands on a page that reads like a meme coin landing. The first 200ms decide trust.
  - **Fix:** default dark-mode users to a sober dark variant (a "lemonade-dark" custom theme that inverts the base ladder but keeps Lemonade Pulp as primary) or to lemonade unchanged with a one-time inline hint that the dark theme is opt-in via the toggle. Keep cyberpunk reachable from the toggle only. The change is in the `head` partial's IIFE.
  - **Suggested command:** `/impeccable colorize internal/server/templates/layout.html`

- **[P1] The primary action has no expectation-setting or in-flight reassurance.**
  - **Why it matters:** the brand promise is speed, and the surface doesn't reinforce it where the click happens. The Create-site button is a bare label; there's no "Usually ready in under a minute" microcopy, no live character counter on the 4096-char prompt, no peek at what success looks like. The user is asked to trust a button.
  - **Fix:** add a `text-base-content/60` caption next to or below the button — *"Usually ready in under a minute. You'll see it build live."* — and a live character counter on the textarea bottom-right. Optionally, a faint thumbnail strip below the button showing three recent generated sites would do more for trust than any copy. The container is the `.card-actions` block (line 62) and the textarea (line 21).
  - **Suggested command:** `/impeccable delight internal/server/templates/landing.html`

- **[P1] Import-site sits at peer visual weight to the main form.**
  - **Why it matters:** two `<details>` elements with identical chrome stack right under the primary card, then a separate POST + a secondary-tinted button. A first-timer's eye gets pulled to "Import an existing site" while parsing the page; this is a power-user affordance that should ride lower and quieter. PRODUCT.md says "power without intimidation: defaults work, advanced is one click away" — import isn't even advanced for the primary user, it's a different flow entirely.
  - **Fix:** demote the import block to a single text link below "View existing apps →": *"Have a Top Banana export? Import a .tar.zst"*, opening either an inline panel or a separate page. Selector: landing.html lines 68–80.
  - **Suggested command:** `/impeccable quieter internal/server/templates/landing.html`

- **[P2] Slug pattern fails silently.**
  - **Why it matters:** `pattern="[a-z0-9]([a-z0-9\-]{1,28}[a-z0-9])?"` produces an opaque native popup on invalid input. A user who types "My Book Club" gets a generic browser-rendered "Please match the requested format" with no guidance on the actual rule.
  - **Fix:** add a live-validating hint region under the input with `aria-describedby` linking to the existing help paragraph, plus a transform preview: as the user types, show *"'My Book Club' becomes 'my-book-club'"* with a confirm-to-use affordance. Selector: `#slug` (line 47).
  - **Suggested command:** `/impeccable harden internal/server/templates/landing.html`

#### Persona Red Flags

- **Jordan (first-timer, desktop):** lands, sees 12 templates equal-weight to the prompt, hesitates — *"do I have to pick one?"*. Doesn't notice "blank" is pre-checked because the radio dot is the same size as the others and selection only adds a 1px border ring. Reads two `<details>` plus an import block before clicking Create. The H1 says "Build a new app" but nothing previews what "an app" looks like coming out of this prompt.
- **Casey (mobile):** the 2-column template grid collapses to 1-column below `sm`, making the page a 14-block vertical scroll (prompt + 12 templates + 2 details + button + import details + apps link). The Create button sits past one full screen on a phone. The textarea is `rows="6"` (fine) but lacks `autocapitalize="sentences"` and `autocomplete="off"`; the slug input lacks both as well. PRODUCT.md explicitly requires mobile usability on this form.
- **Sam (a11y, keyboard + screen reader):** the radio-card selection state is conveyed by color-only — a hairline border + 1px ring in Lemonade Pulp. No `aria-checked` mirror outside the native radio, no SVG checkmark, no text affordance for selected. The slug pattern has no `aria-describedby` pointing at the help paragraph. The pre-paint theme bootstrap script in `layout.html` has no `<noscript>` fallback comment.
- **Mira the book-club organizer (project-specific persona):** her example *is* the placeholder text (*"A small site for my book club where members share what they're reading each month."*) — a real win. But the page never shows her what success looks like; she's asked to trust a button. No "see an example site built from a prompt like this" link, no sample thumbnail.

#### Minor Observations

- `<link rel="preconnect">` to Google Fonts plus a render-blocking `<link rel="stylesheet">` for Inter contradicts the self-hosted-everything stance CLAUDE.md describes (no CDN). Inter could be self-hosted via `@font-face` from `internal/assets/`.
- `<title>` is "Build a new app — Top Banana" — fine. No OG/Twitter meta; a shared link looks naked.
- Footer's *"Your data is yours. Export it anytime."* is a great line buried at `text-base-content/60`. Could surface near the import block to motivate it.
- The textarea omits explicit `spellcheck="true"` and `autocapitalize="sentences"`. Default behavior is fine, but explicit signals intent.
- The inline `<code>{slug}.{{ .Domain }}</code>` pill uses `font-mono text-sm bg-base-200` — exactly DESIGN.md's mono treatment, well done.
- The "blank" template default is hard-coded; the most engaging first build might be a different default (e.g. `landing-page`) since "blank" sets the lowest possible expectation for the AI output.

#### Questions to Consider

1. What if the prompt textarea were the only visible block on first load — templates, slug, and attachments all behind one "More options" disclosure — and the user could ship with just a paragraph and a click? The brand promise is one paragraph plus one button.
2. The brand promise is "a working site in a minute." What if the page *showed* that — a live counter of sites built today, a thumbnail strip of three recent public examples, or a 60-second timer chip next to the button — instead of asserting it in the subhead?
3. The mascot occupies 24px in the header and then disappears. What if she lived inside the empty textarea as a faint watermark that fades on focus, or peeked from the corner of the Create button? One extra place for the wink, none for the noise.
