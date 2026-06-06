---
target: internal/server/templates/login.html
total_score: 18
p0_count: 1
p1_count: 1
timestamp: 2026-06-06T19-38-51Z
slug: internal-server-templates-login-html
---
#### Design Health Score — login.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | No loading state on submit, no "now check your device" copy during the WebAuthn prompt |
| 2 | Match System / Real World | 1 | "passkey bound to your account" is engineer-speak; "bound" isn't a non-coder word |
| 3 | User Control and Freedom | 2 | No "I don't have a passkey on this device" escape hatch |
| 4 | Consistency and Standards | 3 | `autocomplete="username webauthn"` correct; uses head partial; stock daisyUI |
| 5 | Error Prevention | 1 | Only client check is "non-empty"; no email shape check, no debounce, no double-submit guard |
| 6 | Recognition Rather Than Recall | 2 | No recognition affordance for which email the user actually registered |
| 7 | Flexibility and Efficiency | 2 | `autocomplete="username webauthn"` enables conditional UI; no copy explaining it; no Enter→submit feedback |
| 8 | Aesthetic and Minimalist | 3 | Genuinely restrained, borderline anonymous |
| 9 | Error Recovery | 1 | Raw server text concatenated: `'Login failed: ' + ferr` will leak internal messages |
| 10 | Help and Documentation | 1 | Zero teaching about what a passkey is, where it lives, or what to do if this device doesn't have one |
| **Total** | | **18/40** | **Poor — major UX overhaul required** |

#### Anti-Patterns Verdict

**LLM assessment.** Clean of the dumbest auth tells but by underdevelopment, not by intent:

- **Tiny centered card on full-page** — present. `grid place-items-center` + `max-w-md` card on `min-h-screen` is the textbook auth-page-from-a-tutorial silhouette.
- **No padlock/shield/key cliché icon** — good.
- **No "Welcome back" tagline** — good; "Sign in" is honest.
- **No gradient bg** — good; flat `bg-base-100` honors the Flat Rule.
- **`shadow-sm` on the card** (line 9) — the Flat Rule says shadow is reserved for floating UI. A login card isn't floating; the shadow is decorative.
- **Brand voice on first-meeting**: the banana does not smile here. No `{{ template "brand" . }}` header. The mascot, wordmark, and theme toggle are absent — this is a brand-touching surface.
- **Error handling is the giveaway**: `'Could not start login: ' + msg` (line 52) concatenates raw server response text into the alert. The server's 4xx body becomes the user's UI.

**Deterministic scan.** detect.mjs flagged `flat-type-hierarchy` at line 11: "Sizes 14px, 16px, 20px, ratio 1.4:1." Same false-positive shape as workspace.html (the "1.4:1" is the *span* 20/14; adjacent steps are 16/14 = 1.14 and 20/16 = 1.25). Substantively interesting (a stronger display tier on a brand-first surface would help), but not a clean rule violation by the literal definition.

**Visual overlays.** Unavailable.

#### Priority Issues

- **[P0] No recovery path for "this device has no passkey for that email."**
  - **Why it matters:** silent dead-end. User types email → OS sheet says "No matching credential" → dismisses → lands back on the same form with no guidance. They will retry, retype, and leave. The "I'm on a new device" flow is the primary failure mode this page should serve.
  - **Fix:** add a secondary `<a class="link link-hover">` below the button: "I'm on a new device" or "I don't have a passkey here." Wire it to a server flow (email a one-time enrollment link, or page-internal copy explaining device-add).
  - **Suggested command:** `/impeccable harden internal/server/templates/login.html`

- **[P1] Raw server error bodies surface in the user-facing alert.**
  - **Why it matters:** `'Could not start login: ' + msg` (line 52) and `'Login failed: ' + ferr` (line 68) leak internal text. A "no credential found" 4xx body becomes the user's UI. Stack-trace leakage on a 500 is possible.
  - **Fix:** map known statuses (400/401/404/410) to user-facing copy; log raw bodies. Switch the unknown-error path to "Sign-in didn't go through. Try again, or refresh the page."
  - **Suggested command:** `/impeccable clarify internal/server/templates/login.html`

- **[P2] No brand chrome on a brand-touching surface.**
  - **Why it matters:** PRODUCT.md commits "auth flow is brand-touching." The mascot is the brand's smile budget. The page has no `{{ template "brand" . }}` header, no `{{ template "footer" . }}` — the user meets a faceless centered card.
  - **Fix:** add `{{ template "brand" . }}` above `<main>` and `{{ template "footer" . }}` after. Drop the `shadow-sm` on the card. The Flat Rule wins.
  - **Suggested command:** `/impeccable delight internal/server/templates/login.html`

#### Persona Red Flags

- **Sam (a11y / WebAuthn dance):** the `<div hidden role="alert">` toggled by removing `hidden` does not always re-announce in NVDA/VoiceOver. Should be `aria-live="polite"` always-present regions with content swapped. Also: button doesn't get focus-back after error, so a keyboard user is stranded on the alert.
- **Returning user without their passkey here:** the page text says "the passkey bound to your account" — singular. User assumes they have only one. There's no path to add one from a new device.

#### Minor Observations

- `email = emailEl.value.trim().toLowerCase()` is a quiet UX win but no visual feedback that the email was normalized.
- `placeholder="you@example.com"` duplicates the `<span>Email</span>` label. The label is the canonical signal.
- Try/catch dumps `err.message || String(err)` (line 73). On Safari NotAllowedError, this becomes "The operation either timed out or was not allowed." with no remediation copy.
