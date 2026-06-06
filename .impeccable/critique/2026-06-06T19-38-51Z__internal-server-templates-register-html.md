---
target: internal/server/templates/register.html
total_score: 19
p0_count: 2
p1_count: 1
timestamp: 2026-06-06T19-38-51Z
slug: internal-server-templates-register-html
---
#### Design Health Score — register.html

| # | Heuristic | Score | Key Issue |
|---|-----------|-------|-----------|
| 1 | Visibility of System Status | 2 | "Redirecting…" success is nice; no progress between begin/finish/consume (3 async calls behind one click) |
| 2 | Match System / Real World | 1 | "Bind a passkey" — "bind" is API jargon for first-time enrollees |
| 3 | User Control and Freedom | 1 | Once you click you're locked in; no "wrong device" exit, no "resend invite" |
| 4 | Consistency and Standards | 3 | Matches login shell; success/error pattern parallel |
| 5 | Error Prevention | 2 | No `disabled` reset if consume fails after credential creation — credential exists, invite isn't consumed |
| 6 | Recognition Rather Than Recall | 3 | Echoing `{{ .Email }}` in mono confirms which identity the invite is for |
| 7 | Flexibility and Efficiency | 2 | Single button right for the flow; no copy explaining what the OS prompt will look like |
| 8 | Aesthetic and Minimalist | 3 | Same restrained card; clean |
| 9 | Error Recovery | 1 | Expired-invite case invisible; user clicks, gets a 4xx body dumped raw into an alert |
| 10 | Help and Documentation | 1 | No "what is a passkey?", no "what if my device doesn't support this?", no "you'll need this same device to sign in" |
| **Total** | | **19/40** | **Poor — major UX overhaul required** |

#### Anti-Patterns Verdict

**LLM assessment.** Same patterns as login.html — clean by underdevelopment, not by intent:

- **Tiny centered card** — same silhouette.
- **`shadow-sm` on the card** (line 9) — same Flat Rule violation.
- **`data-email` and `data-invite` on `<body>`** (line 7) — the invite token sits in DOM where browser extensions, devtools casual viewers, and `document.body.outerHTML` callers can read it. Footgun.
- **No brand chrome** — no `{{ template "brand" . }}`, no `{{ template "footer" . }}`. This is the brand's first impression for new users. The banana is absent. PRODUCT.md commits "the mascot does the smiling" and "auth is brand-touching"; this page is both, and silent.
- **No passkey teaching** — "Bind a passkey" + one button. Zero explanation of what a passkey is, what the OS prompt will look like, what device this binds to, or how next-time sign-in works. The page assumes the user already knows.
- **Error handling collapses to raw server text** — three concatenation paths (`berr`, `ferr`, `cerr`).
- **`setTimeout(..., 800)`** redirect (line 83) is too fast; the success alert flashes mid-read. 1500ms minimum.

**Deterministic scan.** Same `flat-type-hierarchy` finding as login.html. Same false-positive semantic.

**Visual overlays.** Unavailable.

#### Priority Issues

- **[P0] No passkey teaching at the user's first encounter.**
  - **Why it matters:** this page is the brand's first impression for a new user. "Bind a passkey" reads as cryptographic obligation. The page is silent on what a passkey is, what the OS prompt will look like, and what device this binds to. PRODUCT.md's "friendly, cheeky, capable" promise lives or dies here.
  - **Fix:** add 1–2 sentences in plain language above or near the CTA: "A passkey is your face, fingerprint, or device PIN — there's no password to remember, and it only works on this device (and others you sync with). When you click below, your device will ask you to confirm." Optionally a small "What's a passkey?" disclosure with deeper detail.
  - **Suggested command:** `/impeccable clarify internal/server/templates/register.html`

- **[P0] Credential-created-but-invite-not-consumed is unrecoverable from the UI.**
  - **Why it matters:** the flow has three async steps (`registerBegin` → `navigator.credentials.create` → `registerFinish` → `/register/finish?invite=…`). If `consumeRes` fails after the credential was created, the device has a passkey but the invite isn't consumed. Re-clicking re-runs `registerBegin`, which the server probably rejects (credential already exists). The user sees "Could not finalize:" with raw server text and is permanently stuck.
  - **Fix:** the better path is server-side — `/register/finish` should be idempotent and accept "credential already exists for this email" as a retryable case. Failing that, surface a specific UI path: "We created your passkey but couldn't finalize your invite. [Try again] [Contact support]." Don't let the user re-click `Create passkey` and confuse the relying party.
  - **Suggested command:** `/impeccable harden internal/server/templates/register.html`

- **[P1] Invite token in `data-invite` on `<body>`.**
  - **Why it matters:** any browser extension content script reads `document.body.dataset.invite`. The token is one-time-use and short-lived, but the principle is wrong: secrets don't belong on `<body>`.
  - **Fix:** move the invite token out of the DOM. Either (a) the server stores it in a short-lived HttpOnly cookie at /register render time and the `/register/finish` endpoint reads it from the cookie, or (b) include it via a `<script type="application/json" id="invite-data">` block that's parsed once and removed from the DOM, or (c) submit the token as a hidden form field on POST instead of a query param.
  - **Suggested command:** `/impeccable harden internal/server/templates/register.html`

- **[P2] No brand chrome; mascot absent at the brand's first-impression moment.**
  - **Why it matters:** same as login.html. Per PRODUCT.md "the mascot does the smiling"; register.html is the surface where pride could land for a new user. The success state ("Passkey created. Redirecting…") is the single moment in the file where the brand could smile — it doesn't.
  - **Fix:** add `{{ template "brand" . }}` header + `{{ template "footer" . }}`. Drop `shadow-sm`. Consider a small mascot beat on the success state (44px scale, like the workspace post-build moment). Reuse the `mascot-celebrate` animation that already lives in `app.input.css`.
  - **Suggested command:** `/impeccable delight internal/server/templates/register.html`

#### Persona Red Flags

- **Mira (first-time enrollee, doesn't know what a passkey is):** every concern lives here. "Bind a passkey" reads as cryptographic. No explanation of what the OS sheet will look like. The banana never appears. Pride is impossible because the page never tells her she's done something good — only that she should do it.
- **Riley (denied passkey / cancelled prompt / expired invite):** all three failure modes converge on the same `alert-error` with raw server text. The post-credential-pre-consume gap is the worst UX state in the file.
- **Sam (a11y):** `register-success` uses `role="status"` (correct for polite live-region), but `register-error` uses `role="alert"` — different politeness levels for parallel states. The `hidden` toggling pattern has the same NVDA/VoiceOver announcement issue as login.html.

#### Minor Observations

- The `<span class="font-mono">{{ .Email }}</span>` (line 13) — DESIGN.md reserves mono for slugs/domains/paths. Email is an identifier, not a path. Same call as admin_users.html (which just dropped mono on emails). Consider `font-semibold` instead.
- `setTimeout(..., 800)` is too fast. The success message gets ~0.8s of visibility before the redirect — most users won't read it. 1500ms minimum, or wire the redirect to a button.
- No "What does this button do?" disclosure (`<details>`-style) explaining the OS prompt that's about to appear.
- `data-invite="{{ .InviteToken }}"` — if the token has any URL-special chars, the value may be malformed without HTML escaping (Go templates do escape by default, but worth a note).
