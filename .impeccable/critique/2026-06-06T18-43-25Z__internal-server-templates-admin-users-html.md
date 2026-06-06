---
target: internal/server/templates/admin_users.html
total_score: 24
p0_count: 1
p1_count: 1
timestamp: 2026-06-06T18-43-25Z
slug: internal-server-templates-admin-users-html
---
#### Design Health Score

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 3 | Flash/error alerts + status badges + Copy "Copied!" affordance; no row count on either table, no "saving…" pending state on Quotas Save |
| 2 | Match System / Real World | 3 | "Issue invite," "Disable," "Quotas," "Max apps" all admin-vernacular and correct; "Utility" tier label is the weakest token |
| 3 | User Control and Freedom | 3 | Confirm dialog on destructive paths; Cancel in panel; no undo on triggered Revoke sessions (server contract, not UI) |
| 4 | Consistency and Standards | 3 | Three identical `card bg-base-100 border border-base-300 mb-6` blocks; partially defensible (Invite is editable, Pending/Users are data) |
| 5 | Error Prevention | 2 | Revoke invite / Disable user / Revoke sessions all use the same one-tap dialog with identical friction; the new `data-confirm-slug` ceremony is unused; Quotas free-text models have zero validation |
| 6 | Recognition Rather Than Recall | 2 | Quotas panel's 4 model fields are bare text inputs demanding exact recall of "provider/model" strings; biggest cognitive cost on the page |
| 7 | Flexibility and Efficiency | 1 | No search/filter, no sortable columns, no bulk actions, no keyboard shortcuts; mouse-only on the page that serves the most keyboard-fluent audience |
| 8 | Aesthetic and Minimalist | 3 | Density appropriate for admin; three identical card chromes; 6 data + 3 action columns wrap unevenly; `<th></th>` empty headers are the aesthetic miss |
| 9 | Error Recovery | 2 | No inline validation for duplicate-invite; Quotas accepts any string (invalid model id discovered later by a user when build silently 500s) |
| 10 | Help and Documentation | 2 | Invite paragraph is excellent; no help on what each tier model *is* in product terms; page assumes platform internals knowledge |
| **Total** | | **24/40** | **Acceptable — significant improvements before users are happy** |

#### Anti-Patterns Verdict

**LLM assessment.** Register mostly right. This reads like a real admin page, not a generated dashboard — no hero metric, no gradient, no eyebrow-on-every-section. The drift:

- **`font-mono` on email columns (lines 48, 82) is affected.** DESIGN.md §3 reserves mono for "slugs, custom domains, file paths, inline code." Email is an identifier, not a path. The mono treatment is the AI-tasteful move that says "this looks like code therefore it must be important" — it isn't. Drop to default sans; emails are still distinguishable because they contain `@`.
- **`text-base-content/50` on "default" (line 84) violates the 70/60 Rule explicitly.** DESIGN.md is unambiguous: "Never lower than 60%."
- **Three identical card chromes is the monotony tell.** Same fix that worked on manage.html applies: keep Invite as a card (editable surface), de-card Pending and All Users (they're data tables, already containered via `overflow-x-auto rounded-box border`).
- **The tinted sub-region in the Quotas panel** (line 150: `rounded-box border border-base-300 bg-base-200/40 p-3`) is the **Ladder Rule applied correctly** — keep.
- **No mascot.** Right call — admin surface.

**Deterministic scan.** `detect.mjs` exited 0 with `[]`. Zero findings. Once again the issues are compositional and rule-level, not CSS-shape.

**Visual overlays.** Unavailable — dev server not running and the file is a Go template.

#### Overall Impression

A functional admin surface that scales poorly past ~15 users and asks for typed-string recall in places where autocomplete would erase a class of bugs. The single biggest hidden cost: the four free-text model-id inputs in the Quotas panel. A misspelled model id surfaces as an opaque server 500 days later when a real user tries to build. The page is the only surface where these get set, and it does nothing to prevent typos. The second-biggest: Disable user (a heavy, irreversible-feeling action) uses the same one-tap dialog as Revoke invite (a soft, low-stakes action) — and the `data-confirm-slug` ceremony we just shipped is sitting unused.

#### What's Working

1. **Shared single panel + data-attribute hydration (lines 88-95, 194-213).** Smart. One DOM node, N rows, no per-row form duplication. This is admin-thinking.
2. **Self-row demotion (lines 82, 96).** "you" badge + hidden destructive actions on the current admin's row is exactly the right call. Quiet, correct, prevents the "I disabled myself" footgun.
3. **The Invite paragraph (lines 18-21).** Plain-English, present-tense, useful information density. Brand voice working at the right moment.

#### Priority Issues

- **[P0] Free-text model-id fields invite typo-class outages.** An admin sets `anthropic/claud-haiku-4-5` (missing `e`); the user's next build fails with an opaque server error. **Fix:** render each model input as `<input list="known-models" …>` with a `<datalist id="known-models">` populated from the server's catalog of supported model ids. Keep free-text so power admins can paste novel ids; autocomplete the 95% case. Add a "What's in the catalog?" link beside the legend on line 147.
  **Command:** `/impeccable harden internal/server/templates/admin_users.html`

- **[P1] Disable user should use the typed-email confirmation ceremony.** Disable is materially heavier than Revoke invite (boots an active user, locks them out) but uses the same one-tap dialog. `confirm_dialog` just learned `data-confirm-slug` in the apps pass. **Fix:** lines 102-106, add `data-confirm-slug="{{ .Email }}"` to the Disable form, rewrite body copy to name the user. Keep Revoke sessions on the one-tap flow (recoverable by re-signing in); keep Revoke invite on the one-tap (unredeemed token).
  **Command:** `/impeccable harden internal/server/templates/admin_users.html`

- **[P2] De-card the two data tables; keep the Invite card.** Three identical card chromes flatten visual hierarchy and bury the only editable region. **Fix:** lines 39 and 73, drop the `<section class="card">…<div class="card-body">` wrapper around Pending and All Users. Promote H2s to flush page-level with `mb-3` and a count chip ("Pending invites · 3"). Add `mt-8` between sections.
  **Command:** `/impeccable distill internal/server/templates/admin_users.html`

- **[P2] No filter/search on the Users table.** Alex and Pat operate at 50+ users; scrolling-by-eye is the failure mode. **Fix:** above the table, a single `<input type="search" placeholder="Filter by email or role">` that filters rows client-side. Add a count chip in the H2. Skip bulk actions until there's user signal asking for them.
  **Command:** `/impeccable adapt internal/server/templates/admin_users.html`

- **[P3] Mono on email columns + `/50` on "default" violate the type rules.** **Fix:** drop `font-mono` from lines 48 and 82. Change `text-base-content/50` on line 84 to `text-base-content/60` and consider rendering `—` instead of "default" so the meaning is shape-conveyed.
  **Command:** `/impeccable polish internal/server/templates/admin_users.html`

#### Persona Red Flags

- **Alex (super-admin power user):** no keyboard shortcuts to open Quotas on the focused row (`q` would do it), no `/` to focus a future filter input, no sortable columns. Mouse-only on the page serving the most keyboard-fluent audience.
- **Sam (a11y):** `<th></th>` empty headers on lines 44 and 78 are screen-reader hostile — give them `scope="col"` + `<span class="sr-only">Actions</span>`. The Quotas trigger button has no aria description of *which user* it acts on; screen-reader users hear "Quotas" five times in a row. Add `aria-label="Edit quotas for {{ .Email }}"`.
- **Pat the platform operator (project-specific):** knows `anthropic/claude-haiku-4-5` but mistypes it once a month. Wants a datalist. Also wants to know which models the system *supports* before saving — a stale custom override that no longer resolves should surface as a warning on the row, not as a silent runtime failure.

#### Minor Observations

- **Pending invites has no "Resend" action**; if the URL is lost the admin must revoke and re-issue.
- **The Quotas panel sticky footer** is good. Consider a "Reset to defaults" ghost button between Cancel and Save.
- **The invite Role select** shows admin selected by default (line 30) — surprising default if most invites are non-admin in real usage. Consider `member`.
- **`<td>{{ .Expires }}</td>`** (line 50) renders a server-formatted string; wrap in `<time datetime="…" title="…">` for hover-exact-timestamp.
- **"Invite URL" column** with `break-all <code>` makes the table jaggy at narrow widths. Consider truncating to host + ellipsis with the Copy button doing the actual work.

#### Questions to Consider

1. **If the Users table is the operating surface for life-or-death platform actions (disable, revoke), why is it the column-densest, least-searchable page in the app?** Should the Users table itself be a side-panel detail-view triggered from a much sparser list?
2. **The Quotas panel asks for four model strings.** Should Top Banana ship *system-default profiles* (Frugal / Balanced / Quality) as the primary control and treat per-tier overrides as a `<details>` advanced disclosure? Demotes typo-risk; turns the page into one Select per user instead of four inputs.
3. **Is the email column being mono-styled a tell** that the team subconsciously treats emails as identifiers in the codebase? If so, the mono is honest engineering — and the type-rule violation is actually pointing at a product-modeling question (do we want a username distinct from email?).
