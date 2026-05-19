---
{
  "label": "SaaS pricing",
  "description": "Three-tier pricing comparison with features and CTAs per tier.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1", "$"],
      "message": "pricing pages need a clear <h1> and at least one price (with a $ symbol)"
    }
  ],
  "setup_notes": "Stripe is optional — configure only if you want the CTA buttons to take real payments.\n\nPer-tier Payment Links (recommended for most pricing pages):\n1. Create one Payment Link per tier at https://dashboard.stripe.com/payment-links\n2. In the workspace, replace each `https://buy.stripe.com/REPLACE_ME_<tier>` placeholder in index.html with the matching URL.\n\nDashboard-managed Pricing Table (alternative, if you want to edit prices in Stripe instead of HTML):\n1. Create a Pricing Table at https://dashboard.stripe.com/pricing-tables\n2. Replace `prctbl_REPLACE_ME` and `pk_live_REPLACE_ME` in index.html with your pricing-table-id and publishable key.\n\nThe page will show Stripe errors until real IDs are filled in."
}
---
Site type: SaaS pricing page.

- index.html is a one-page pricing comparison. Inline CSS only.
- Sections: hero (`<h1>` + one-line value prop), three-tier pricing grid, optional FAQ, footer CTA.
- Tiers default to Starter / Pro / Enterprise. Each tier has: name, price (with `$`), short description, bulleted feature list, CTA button. Highlight the middle tier as "most popular".
- If the user describes a specific product, replace placeholder feature lists with realistic features for that product.
- Show prices with the `$` symbol (the lint check enforces this).

Stripe integration (only when the user asks to take real payments):

- Default: Payment Link CTAs. Replace each tier's placeholder `href="#"` with a Stripe Payment Link of the form `https://buy.stripe.com/REPLACE_ME_<tier>` (e.g. `_starter`, `_pro`, `_enterprise`). Keeps the hand-styled grid intact — only the CTA buttons go live. Add a visible callout block at the top of the page (one the user can delete later) telling them to create one Payment Link per tier in the Stripe Dashboard → Payment Links and paste the URLs in.
- Alternative: `<stripe-pricing-table>`. If the user wants pricing managed centrally in Stripe Dashboard rather than hand-edited in HTML, replace the entire 3-tier `<main>` grid with:
  ```html
  <stripe-pricing-table
    pricing-table-id="prctbl_REPLACE_ME"
    publishable-key="pk_live_REPLACE_ME">
  </stripe-pricing-table>
  <script async src="https://js.stripe.com/v3/pricing-table.js"></script>
  ```
  Keep the hero (`<h1>` + value prop) above the widget, and keep at least one `$` somewhere in the hero copy (e.g. "starts at $0/mo") so the static lint check still passes before Stripe renders.
- Never invent realistic-looking Stripe IDs (no `prctbl_1AbCd…`, no `https://buy.stripe.com/abcdef`). Always use the `REPLACE_ME` placeholders so the user knows what to swap in.
