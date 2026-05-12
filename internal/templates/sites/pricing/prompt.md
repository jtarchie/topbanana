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
  ]
}
---
Site type: SaaS pricing page.

- index.html is a one-page pricing comparison. Inline CSS only.
- Sections: hero (`<h1>` + one-line value prop), three-tier pricing grid, optional FAQ, footer CTA.
- Tiers default to Starter / Pro / Enterprise. Each tier has: name, price (with `$`), short description, bulleted feature list, CTA button. Highlight the middle tier as "most popular".
- If the user describes a specific product, replace placeholder feature lists with realistic features for that product.
- Show prices with the `$` symbol (the lint check enforces this).
