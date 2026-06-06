# Product

## Register

product

## Users

Curious non-coders shipping a one-off site: hobbyists, small-business owners, event organizers, club leaders, instructors. They arrive with an idea in plain English ("a site for my book club", "a landing page for the bake sale") and want a working, hostable site in a minute or two. They are not engineers, but they are not afraid of a settings page either: they will manage a custom domain, edit files, and upload references if the UI makes that legible. Their context when using Top Banana is usually a single focused session at a desk or laptop, not on the go. The job to be done is: describe an idea, get a real site, share its URL.

A meaningful secondary audience: developers using Top Banana as a sketchpad for static sites. They benefit from the same clarity but tolerate (and want) the lower-level affordances — raw files, slugs, exports, custom domains. The product is not designed for them; it accommodates them.

## Product Purpose

Top Banana turns a prompt into a hosted static site. It exists because the gap between "I have an idea" and "I have a live URL" should be a minute, not an afternoon. Success looks like: a non-engineer types a paragraph, watches the build stream, opens their slug subdomain, and forwards the URL to a friend — without ever opening a code editor or thinking about hosting. The platform's first-party UI is a small set of tool surfaces (landing/build form, apps list, workspace, manage, account, login/register, system, admin) that exist to set up, run, and steward those generated sites.

The brand promise is speed and "it just worked." The interface's job is to make that promise visible and to keep the surprise positive — the banana is a wink, the platform is real.

## Brand Personality

Friendly, cheeky, capable. Three-word target.

A confident toy. The mascot smiles; the build pipeline is industrial. Voice is warm and direct, with wit kept in the labels and microcopy, not in the chrome. No corporate solemnity, no childish chaos. The banana is the only mascot; the rest of the surface is sober workshop tooling tinted lemonade-yellow. Emotionally, the user should feel "this is going to be fun, and it's actually going to work" — never "this is a toy I can't trust" or "this is a serious tool that's going to lecture me."

## Anti-references

The interface should NOT look like any of these, and the design system should actively avoid drifting toward them:

- **Generic SaaS dashboard chrome.** Linear / Vercel / Stripe-clone gradient-hero-plus-feature-card-grid templates. The hero-metric pattern (big number + small label + supporting stats + gradient accent), the identical card grid, the tiny uppercase tracked eyebrow above every section. If the page could ship under another SaaS logo without anyone noticing, it's wrong.
- **Childish or cluttered toy aesthetics.** Scratch, MIT App Inventor, Toca Boca. Colorful chaos. Round-everything, comic-sans-adjacent, "fun" as visual noise. The banana mascot is the budget for whimsy; spending it on layout undoes the brand.
- **Stiff enterprise admin.** Jira / Confluence / ServiceNow. Cold gray, dense nested settings, twelve tabs deep, opaque labels. Even the "Manage" and "Admin" surfaces should feel like a workshop, not a console.
- **Crypto / web3 dark-neon aesthetics.** The cyberpunk dark theme exists as a daisyUI palette, not as a brand statement. No glow, no holographic gradients, no aggressive monospace, no "$BANANA" energy.

## Design Principles

1. **The mascot does the smiling. The chrome stays sober.** Yellow primary, banana favicon, and one cartoon mark carry all the personality. Layout, spacing, and hierarchy stay precise and grown-up so the wink lands instead of becoming the whole joke.
2. **Speed is the brand promise; the UI must visibly serve it.** Every primary screen should make the next click obvious within one second of landing. The build form is the lede; everything else is folded behind disclosure (`<details>`, sub-nav, settings) so first-timers see one button, not a menu.
3. **Power without intimidation.** Slugs, attachments, custom domains, exports, and admin tools exist and are first-class — but they ride below a friendly default. Progressive disclosure is the lever: defaults work, advanced is one click away, names are honest.
4. **Show, don't tell.** Generated sites are the demo. The first-party UI does not need marketing copy ("supercharge your...", "world-class...") — its job is to get users to a real site as fast as possible. Microcopy is specific, concrete, and present-tense.
5. **Warmth is carried by color, not by softness.** Lemonade yellow, Inter, generous spacing, real borders. No fuzzy gradients, no glassmorphism, no cream-tinted "warm" near-whites used as a default surface. If a screen feels too cold, it's because the hierarchy is thin — fix it with type and rhythm, not with a wash of beige.

## Accessibility & Inclusion

WCAG 2.2 AA baseline, enforced on the first-party admin UI:

- Body text ≥4.5:1 against its surface; large text (≥18px or bold ≥14px) ≥3:1. Placeholder text held to the same body bar — daisyUI defaults must be checked, not trusted.
- Full keyboard navigation on the build form, apps list, workspace, manage, account, and admin surfaces. Visible `:focus-visible` rings (already declared in `app.input.css`).
- `prefers-reduced-motion: reduce` honored on every animated transition — including the side-panel slide-in pattern and any future build-stream motion.
- Theme parity: both the `lemonade` (light) and `cyberpunk` (dark) daisyUI themes must hit AA. The pre-paint theme bootstrap script in `layout.html` already prevents the light→dark flash; new themes added to Theme Studio must be vetted the same way.
- Color is never the only signal. Status (building / live / failed), validation errors, and active-tab states all carry a text or icon affordance in addition to color.
- Mobile usability matters for the prompt form specifically — non-coders may start a build from a phone. The full Manage surface can degrade gracefully on small screens but the build form must work.

Out of scope as formal targets (but worth keeping aesthetically): screen-reader transcripts for the build stream, full localization, AAA contrast.
