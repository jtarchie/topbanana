# SaaS pricing

## Purpose
A three-tier pricing comparison page — hero, feature-list-per-tier with prices and CTAs. The middle tier highlights as "most popular." Picked when the user wants a dedicated pricing page (not a section embedded in a landing page).

## What ships
- `skeleton/index.html` — three DaisyUI `card`s in a responsive grid.

## Checks
- `index.html` must contain `<h1` and a `$` symbol — every pricing page needs a headline and at least one visible price.

## Config
Stripe integration is optional. Two modes the agent picks between based on user intent:
- **Payment Link CTAs (default)** — keeps the hand-styled grid; each tier's CTA `href` becomes a `https://buy.stripe.com/...` URL.
- **Stripe Pricing Table widget** — replaces the grid with `<stripe-pricing-table>`, sourcing prices from the Stripe Dashboard.

Both modes use placeholder IDs (`REPLACE_ME`, `prctbl_REPLACE_ME`, `pk_live_REPLACE_ME`) the end user fills in. The `setup_notes` field surfaces the manual steps on the manage page.

## Gotchas
The `$` check is satisfied differently depending on the Stripe mode. With Payment Links, the prices stay in the HTML and the check is trivially satisfied. With Pricing Table, the agent is instructed to keep at least one `$` in the hero copy (e.g. "starts at $0/mo") so the static check still passes before Stripe renders the iframe.
