---
target: internal/server/templates/manage.html
total_score: 25
p0_count: 2
p1_count: 2
timestamp: 2026-06-06T16-45-43Z
slug: internal-server-templates-manage-html
---
#### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Flash alert covers Save outcomes; Save button has no busy state; no "unsaved changes" indicator on dirty toggles |
| 2 | Match System / Real World | 3 | "Custom web address" is plain-English win; "Build transcripts" is jargon; "Allow other websites to post data here" is decent but "webhook" only appears in the caption |
| 3 | User Control and Freedom | 2 | No Cancel/Reset on the settings form; toggling a permission and navigating away silently discards |
| 4 | Consistency and Standards | 2 | Four POST endpoints (`/settings`, `/manage/remix`, `/apps/transfer`, `/settings/delete`) live in one stack of identical cards; the layout actively *hides* the form boundary |
| 5 | Error Prevention | 2 | Risk calibration is inverted: Transfer (reversible by mutual agreement) gets a modal; Delete (irreversible) gets only type-the-slug; Duplicate (creates a new app, costs quota) has zero guard |
| 6 | Recognition Rather Than Recall | 3 | Section H2s are honest; the General `dl` shows Name read-only with no obvious path to rename — user has to recall that rename lives in Workspace |
| 7 | Flexibility and Efficiency | 3 | CSV/JSON join-buttons are good; no keyboard shortcut for Save; no inline edit on Name |
| 8 | Aesthetic and Minimalist Design | 2 | Seven identical `card bg-base-100 border border-base-300` blocks stacked vertically; "Advanced tools" has cards-inside-cards (the literal absolute-ban shape) |
| 9 | Error Recovery | 2 | DNS guidance is great proactive help; nothing comparable on Transfer (what if the email isn't a registered user?) or Delete (what about the custom domain — is it released?) |
| 10 | Help and Documentation | 3 | DNS paragraph and Duplicate caveats are concrete and present-tense; no docs link, no "what counts as a submission?" tooltip |
| **Total** | | **25/40** | **Acceptable — significant improvements before users are happy** |

#### Anti-Patterns Verdict

**LLM assessment.** This page is **register-correct and tonally sober**. No eyebrows, no numbered scaffolding, no gradient, no hero metric, no emoji. The mascot stays in the navbar. The type ladder (30 → 18 → 14) is in spec. That's the good news.

The bad news is **layout sameness**:

- Seven `card bg-base-100 border border-base-300` blocks stacked in a `max-w-3xl` column with identical body padding and identical `card-title text-lg` H2s. The Delete card's `border-error/40` is the only visual differentiation in 213 lines of stacked sections. A design director should be able to tell a destructive action from a read-only `<dl>` from a data table by glance, not by reading the H2.
- The "identical card grids" absolute ban targets the icon+heading+text tile-grid pattern (Linear/Stripe feature cards). A stacked single-column settings page is a legitimate variant. **But** lines 154-167 — the "Advanced tools" 2-column grid of link-cards (`All files →` / `Build transcripts →`, same size, `font-medium` heading + `text-xs` caption + arrow + hover-border) — is the literal cliché the ban names. Same size, same shape, hover-border-primary identical. That's a tell.
- **The mascot does not smile on this page.** Manage is the surface where a non-coder might be most anxious (custom domain, transfer, delete) and most relieved (submissions came in). The brand promise is "friendly, cheeky, capable" — Manage is 3-of-3 capable, 0-of-3 friendly. That's the right ratio at Delete; it's a missed beat at Duplicate, at the Form Submissions empty state, and at the Save-success flash.

**Deterministic scan.** `detect.mjs --json internal/server/templates/manage.html` exited 0 with `[]`. Zero findings. No banned CSS shapes are present. The detector and the design review converge on the same conclusion: this page passes every catchable rule, and the issues that remain are compositional — form-boundary clarity, risk-escalation calibration, information architecture, and the one literal identical-card-grid violation that no shape detector flagged because it's only 2 tiles.

**Visual overlays.** Unavailable. Dev server isn't running and the file is a Go template; static load would render `{{ ... }}` as text.

#### Overall Impression

A settings page that respects the design system everywhere it could be wrong on details — and still leaves the user uncertain about which Save button saves what. The biggest hidden cost is the **invisible form boundary**: one `<form>` ends at line 96 with a right-aligned "Save changes" button, then four more `<form>` elements live inside visually identical cards. The user has no way to tell by glance which controls belong to which submit. This is the single thing to fix; almost everything else trails from it.

The second-biggest opportunity: **risk calibration is inverted.** Transfer (which both parties can technically undo) goes through a modal. Delete (truly irreversible) gets only type-the-slug. Duplicate (creates a new app, may surprise users on quota) has nothing. The current pattern teaches users that the modal IS the warning — then Delete bypasses it.

#### What's Working

1. **The DNS paragraph at line 47.** Concrete, present-tense, names the actual landmine ("Don't use Cloudflare's orange-cloud proxy — set the record to 'DNS only' or certificate issuance will fail."). This is the brand voice working at the exact moment a non-coder needs it.
2. **Toggle row pattern (lines 60-89).** Full label as hit target, bold action + `/60` caption per row, no eyebrows, no icons. Exemplary daisyUI use; the rest of the system should look like this.
3. **Delete type-to-confirm (lines 199-213).** Type-the-slug is the right pattern for irreversible destruction; placeholder + label both show the slug; `autocomplete="off"` is correctly set.

#### Priority Issues

- **[P0] The form boundary is invisible.**
  - **Why it matters:** the single `<form>` (lines 31-96) holds General + Permissions and ends with one right-aligned "Save changes." Below that, four more `<form>` elements live inside identical cards. A user toggling a permission and then filling in the Transfer email cannot tell which submit applies. "Does Save changes save my Transfer email I haven't filled in yet? Does it save the toggles? Does it save Form submissions filters?" The `</form>` boundary at line 96 is invisible.
  - **Fix:** move "Save changes" *into* the Permissions card body (so the button visually closes the form section), or pair it with a sticky save-bar at the bottom that activates when the form is dirty. Visually de-card Duplicate / Transfer / Delete so they don't mimic the General/Permissions chrome — e.g. drop the card to a plain `<section>` with `border-t border-base-300` and `pt-8` for separation.
  - **Suggested command:** `/impeccable clarify internal/server/templates/manage.html`

- **[P0] Risk-escalation is miscalibrated.**
  - **Why it matters:** Duplicate (creates a new app, costs quota, can surprise) has zero guard; Transfer (reversible by mutual agreement) goes through a modal; Delete (irreversible) has only type-the-slug. The current pattern teaches users that the modal IS the warning — then Delete bypasses it. The most destructive action has the lightest guard.
  - **Fix:** add `js-confirm` to Duplicate ("Make a copy of this site? You'll own both. Quota will count the copy."); add `js-confirm` + type-the-slug to Delete as defense in depth (modal asks intent, slug confirms identity, the page warns that custom domains will be released).
  - **Suggested command:** `/impeccable harden internal/server/templates/manage.html`

- **[P1] Advanced tools is the literal "identical card grid" ban.**
  - **Why it matters:** lines 154-167 are two same-sized cards with `font-medium` heading + `text-xs` description + arrow, hover-border-primary. This is the cliché the absolute ban targets. Two tiles doesn't change the shape; the page now has cards-inside-cards (the link-cards sit inside a parent settings card), which DESIGN.md prohibits explicitly.
  - **Fix:** convert to two link rows: a `<ul>` of `<li>` with chevron icon + label + caption inline. Or two `btn btn-ghost` rows with right-aligned arrow. Recover the visual budget.
  - **Suggested command:** `/impeccable distill internal/server/templates/manage.html`

- **[P1] Form submissions is buried as section #5.**
  - **Why it matters:** on routine return visits, submissions data is the lede — the user comes to Manage to glance at form data. Burying it behind General + Permissions + Save makes a data-checking visit two scrolls of friction. The settings/data inversion compounds with the form-boundary problem.
  - **Fix:** option A — promote Form submissions to immediately after the Flash alert, above General. Option B — pin a compact "N new submissions" indicator into the page header so it's glanceable without scrolling. Option C — split into a separate subnav tab (per Provocative #2 below; bigger move).
  - **Suggested command:** `/impeccable layout internal/server/templates/manage.html`

- **[P2] Card monotony — seven identical chrome rectangles.**
  - **Why it matters:** Even after the P0/P1 fixes, the page is a stack of `card bg-base-100 border border-base-300`. Texture without rhythm. The Ladder Rule has a perfectly good answer: stratify the cards by what the user *acts on* vs. *reads*.
  - **Fix:** keep full card chrome on the things the user acts on (General, Permissions, Transfer, Delete). Drop secondary sections (Submissions, Advanced tools) to plain bordered `<section>` blocks with H2s flush-left, no card-body padding. Or switch their background to `bg-base-200` so the lightness ladder distinguishes them. Reserve "card-on-base-100" for active edit surfaces.
  - **Suggested command:** `/impeccable polish internal/server/templates/manage.html`

#### Persona Red Flags

- **Mira (returning organizer adding a custom domain):** lands on Manage, scans for "domain," finds it in card #2 — wins. But the textarea has no inline validation (lines 44-46): pasting `https://example.com/` is silently accepted or rejected with no feedback. After Save, the green flash fires but there's no indication of which domain is live vs pending TLS issuance.
- **Sam (a11y on toggles + type-to-confirm):** toggles are correctly wrapped in `<label class="cursor-pointer">` — keyboard and screen reader pickup is fine. But the "Type *{slug}* to confirm" pattern at line 206 shows the slug inside a `<code>`; screen readers read it inline with the surrounding sentence and may sound like "type Slash" or run the slug onto adjacent words. The form has no client-side check that the typed value matches before submit — a screen-reader user who mistypes gets no feedback until the server responds.
- **Riley (stress tester):** Transfer submitted with the current owner's email — no client check. Transfer to an email that isn't a registered user — what happens? Page doesn't say. Delete on a site with active custom domains — does the user know those will be released? Currently no copy hints at it. Permissions toggles flipped while the textarea is dirty — Save commits both; the user may have meant only one.

#### Minor Observations

- **Line 181:** `mb-6` on the Transfer card is the only bottom-margin override on any card — every other section relies on the `space-y-8` on `<main>`. Remove it; it breaks rhythm above Delete.
- **Line 200:** `card-title text-error text-lg` — `card-title` already implies a size in daisyUI; the other H2s also override with `text-lg` so the dual class is technically consistent, but worth picking one pattern across all of them.
- **Line 194 vs line 94:** Transfer's submit is `btn btn-primary btn-sm`; Save changes is `btn btn-primary` (default 2.5rem). Same intent, two sizes. Pick one. (Apply change in workspace.html was just fixed for the same reason — keep the system consistent.)
- **Line 47:** the textarea help paragraph could `<code class="font-mono">example.com</code>` for parallelism with the Delete card's `<code>` slug.
- **Permissions when `FunctionsByTmpl` is true (line 56-58):** the paragraph plus 2 toggles becomes an asymmetric block. Could move the "Always on" note up next to a disabled toggle row so the visual rhythm stays even.
- **No `tabindex` or auto-focus on the Manage page.** Less urgent than workspace, but the General → Custom web address textarea is the most-edited control; could be the focus default.

#### Questions to Consider

1. **Should this page be two pages?** "Site settings" (General, Permissions) and "Site tools" (Submissions, Files, Transcripts, Duplicate, Transfer, Delete) live different lives. The first is edited rarely and saved as a unit; the second is browsed and acted-on individually. Merging them is what forces the seven-card monotony.
2. **Why is Form submissions inside Manage at all?** It's *data*, not configuration. A user opening Manage to see submissions is using this page as a data dashboard because there's nowhere else. If Submissions earned its own subnav tab next to Workspace / Manage, the entire Manage page becomes legibly *settings*, and the duplicate-data-table problem at sites with 1000 submissions disappears too.
3. **Is the Delete section bottom-of-page because that's safe, or because no one wanted to design the placement?** Putting Delete *above* Transfer (most-destructive first, by risk magnitude) is a real option — it inverts current convention but makes "the page ends with the answer to most user intents" instead of "the page ends with the nuclear button."
