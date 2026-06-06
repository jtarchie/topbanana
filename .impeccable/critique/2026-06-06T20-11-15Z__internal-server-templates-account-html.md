---
target: internal/server/templates/account.html
total_score: 20
p0_count: 2
p1_count: 2
timestamp: 2026-06-06T20-11-15Z
slug: internal-server-templates-account-html
---
#### Design Health Score — account.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visual hierarchy | 2 | "Signed in as ..." reads as footer, not subtitle; passkey card title competes with action button in flex row |
| 2 | Information architecture | 1 | Two stacked cards + a naked sign-out form; no settings spine |
| 3 | Typography discipline | 3 | Tight scale, mono on the right things |
| 4 | Color discipline | 3 | One Yellow respected; /70 and /60 correctly used |
| 5 | Spacing and rhythm | 2 | `mb-6` rhythm between cards is the manage.html-before-critique pattern |
| 6 | Affordance clarity | 1 | Sign out as ghost btn-sm at bottom of main; no per-credential revoke; one destructive thing visually equal to Cancel |
| 7 | Density / progressive disclosure | 2 | MCP super-admin env-var copy lives in the user-facing card |
| 8 | Accessibility | 2 | Credential rows have no accessible name beyond a base64url ID; `role="alert"` on permanently empty containers |
| 9 | Brand register fit | 2 | Mascot-off in body is right; chrome doesn't carry "settings + security" register |
| 10 | Resilience | 2 | Add Passkey flow has inline alerts; "lost a device" flow doesn't exist |
| **Total** | | **20/40** | **Poor — major UX overhaul required** |

#### Anti-Patterns Verdict

**LLM assessment.** Not slop in chrome (no eyebrows, no gradients, no hero-metric, no numbered scaffolding). The slop is **structural**: this is the security home of the account, and it presents as a two-card scratchpad with a footer-grade "Sign out" ghost button at the bottom of `<main>`. Register is undercalibrated.

The page can't do the two most-expected security things:

- **No "Sign out everywhere" / "Revoke all other sessions."** A user who lost a laptop has no first-party path. The `confirm_dialog` partial supporting `data-confirm` and `data-confirm-slug` was built for moments like this; the page doesn't even load the partial.
- **No "Delete account."** PRODUCT.md's anti-references demand "your data is yours, export anytime" and the privacy policy commits to deletion-on-request. The UI is silently refusing to honor that — currently, the answer is "email someone."

The passkey list is opaque: raw base64url credential IDs printed in mono. A non-coder cannot tell which device the first-row passkey belongs to, cannot revoke a row, cannot name a row, and cannot tell whether a row is the credential they're currently signed in with.

The MCP card stacks three audiences in one body — standard user, disabled-state user, and super-admin operator with bash env-var explainers — all under "Connect Claude Code." For Mira this is jargon noise; for the developer secondary audience, it's the most interesting thing on the page. It needs disclosure-folding.

Mascot-off in the body is the **right** register call. Account is brand-touching but security-serious; the brand header carrying the mascot is enough.

**Deterministic scan.** Clean (0 findings).

**Visual overlays.** Unavailable.

#### Overall Impression

The most surprising score of the session. Earlier surfaces with comparable scores (login 18, register 19) were small auth shells that just needed teaching copy and recovery paths. account.html is the opposite — a page with real complexity that gets the chrome and mascot-discipline right but ships zero of the page's load-bearing functions. The destructive zone is missing entirely; the passkey list is decorative; the MCP card serves the wrong audience.

The hard truth: two of the P0/P1 fixes need server-side endpoints that probably don't exist yet (per-credential revoke + rename; "sign out everywhere"; delete account). Those are real engineering scope, not polish. Want to know about this before committing to anything.

#### What's Working

1. **Inline error/success pattern for Add Passkey.** `#add-error` + `#add-success` with `role="alert"` / `role="status"`, sober reload after 600ms. The one place on the page that gets the UX shape right.
2. **Mascot-off in the body.** Brand-touching surface that doesn't ham it up. Right register call.
3. **MCP super-admin branch is gated on `IsSuperAdmin`.** The role-aware disclosure instinct is correct — just rendered in the wrong shape.

#### Priority Issues

- **[P0] No destructive zone — no "Sign out everywhere", no "Delete account."**
  - **Why it matters:** The two most-expected security actions are absent; the one destructive action on the page ("Sign out" current session) is dressed as a ghost button. A user who lost a device, or who wants to leave the platform, has no first-party path. The privacy policy promises deletion; the UI refuses to honor it.
  - **Server-side dependency:** "Sign out everywhere" needs an endpoint to revoke all sessions for the current user. "Delete account" needs an irreversible-deletion endpoint (probably with export prompt). The session-revoke API may already exist (the admin "Revoke sessions" form on admin_users.html targets `/admin/users/{email}/sessions/revoke`); a user-facing "all my sessions" endpoint may need adding.
  - **Fix (UI part):** add a "Danger zone" card with three rows: Sign out (current); Sign out everywhere (`js-confirm` + tone error); Delete account (`js-confirm` + `data-confirm-slug="{{ .Email }}"` + tone error). Load the `confirm_dialog` partial.
  - **Suggested command:** `/impeccable harden internal/server/templates/account.html`

- **[P0] Passkey list is opaque: raw IDs, no revoke, no rename, no current-credential marker.**
  - **Why it matters:** Non-coder cannot reason about which device a base64url credential ID belongs to. Sam (screen reader) hears each row as a 43-character random string. Alex (power user) can register a YubiKey but can't name it, can't revoke the old laptop's credential after selling the laptop. The flow that motivates buying a hardware key is the flow this page doesn't support.
  - **Server-side dependency:** per-credential revoke needs an endpoint (probably `/account/passkeys/{id}/delete`). Rename needs a `Nickname` field on the credential record (migration) and an endpoint. Current-credential detection needs the session to expose the credential ID it was minted from.
  - **Fix (UI part):** demote the ID to a `text-xs font-mono text-base-content/60` secondary line; lead the row with "Passkey added {{ .Created }}" (or `Nickname` when available). Per-row Rename + Revoke. Mark current credential with `badge badge-outline` "current session." Disable Revoke on last credential.
  - **Suggested command:** `/impeccable clarify internal/server/templates/account.html`

- **[P1] Layout sameness — two stacked cards with no settings spine.**
  - **Why it matters:** Account reads as "two cards then a link" instead of as a settings surface. `mb-6` rhythm between sibling cards is the manage.html-before-critique pattern. For a page edited rarely, "what categories exist on my account?" should be answerable in one glance.
  - **Fix:** collapse to a single `<section class="card">` with grouped `<div>` rows separated by `border-t border-base-300`, each row being label + control; or keep multi-card but add `<h2>` at title weight per section and use anchor links from the page header. UI-only; no server work.
  - **Suggested command:** `/impeccable layout internal/server/templates/account.html`

- **[P1] MCP card register mismatch — user-facing card carries super-admin env-var copy.**
  - **Why it matters:** Three audiences stacked in one body. For Mira this is jargon noise; for the secondary developer audience, it's the most interesting thing on the page. UI-only; no server work.
  - **Fix:** move the entire MCP block behind `<details><summary>Developer tools</summary>...</details>`. Wrap super-admin operator copy in a nested `alert alert-info`. Rewrite lede to "Top Banana can talk to Claude Code via MCP."
  - **Suggested command:** `/impeccable distill internal/server/templates/account.html`

- **[P2] "Signed in as ... (admin)" reads as footer, not subtitle.**
  - **Why it matters:** The most identifying thing on the page is set at `/70` body weight. The `(admin)` role is parenthetical instead of a badge. UI-only.
  - **Fix:** small identity strip — H1 "Account" + flex row with email in mono `text-sm` and role as `badge badge-ghost text-xs`.
  - **Suggested command:** `/impeccable typeset internal/server/templates/account.html`

#### Persona Red Flags

- **Mira (curious non-coder):** lands here, sees `c29tZS1iYXNlNjR1cmwtaWQ=`-style monospace IDs as her own passkeys, sees a `claude mcp add` command she didn't ask for, sees "Sign out" the size of a Cancel link. Closes the tab thinking the account page is "for developers."
- **Sam (a11y):** each `<li>` in the credentials list has no accessible name beyond the credential ID; a screen reader reads "list item, 43-character random string, added Jan 14." No way to act on a row. No labelled list (`aria-label="Your passkeys"`).
- **Alex (power user adding a YubiKey):** can register a new passkey but can't *name* it "Work YubiKey," can't see "which device am I on now," can't revoke the old laptop's credential after selling the laptop.

#### Minor Observations

- Two separate `<script>` IIFEs at the bottom; the `.js-copy` helper duplicates a pattern in admin_users.html. Extract into a shared partial.
- "Add another passkey" presupposes there's already one; use "Add a passkey" on the empty branch.
- `setTimeout(..., 600)` reload after a successful add discards the inline `#add-success` before the user can read it. Lengthen to 1500ms or prepend the new row in JS.
- `<div id="add-error" role="alert">` starts permanently empty; screen readers may announce spuriously. Use `aria-live="assertive"` and drop `role="alert"` on the empty container.
- Line 2 hardcodes `data-theme="lemonade"` — same as every page, but the pre-paint bootstrap overwrites it. Pull the SSR'd attribute and let the bootstrap own it.

#### Questions to Consider

1. **If a user calls support saying "my laptop was stolen, please log it out," is the first-party answer "we can't from the UI"?** If yes, this page is shipping a known security hole as design. If no, the actual answer (sign out everywhere) deserves to be on this page in `btn-error` weight, not in a support inbox.
2. **Account pages are the one place in a product where users intentionally come to leave.** Top Banana has no Delete account button anywhere. Is that the privacy-policy claim ("your data is yours") that the UI is silently refusing to honor, or is delete a known-pending feature? Either answer changes whether the danger-zone section should ship a real form or a "Coming soon — email us" placeholder. There is no third option where the absence is fine.
