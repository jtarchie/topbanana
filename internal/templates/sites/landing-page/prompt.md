---
{
  "label": "Landing page",
  "description": "Marketing page with a hero, value prop, features, and a clear call-to-action.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "landing pages need a clear <h1> headline in index.html"
    }
  ]
}
---
Site type: landing page / marketing page.

Common patterns (pick what fits the product; sections are optional, not a checklist):
- Sticky navbar with product/brand name and a primary CTA on the right.
- Hero: headline (`<h1>`), one-line value prop, primary CTA button, optional secondary CTA. Optional `mockup-window` / `mockup-browser` / `mockup-phone` showing the product.
- Social proof — logo strip, testimonial cards, stat counters (`stats` component).
- Features / benefits — 3 to 6 items. Each with an inline SVG icon, a heading, and a one-line description. `card` surfaces in a grid.
- Pricing snippet (only if the user mentions pricing).
- FAQ as DaisyUI `collapse` items (only if relevant).
- Final CTA section before footer — bold headline + button.
- Footer with links + copyright.

Aesthetic bar: this should sell. Treat the page like a real marketing site, not a brochure.

- Pick a `data-theme` that matches the product's tone. SaaS / dev tools: `corporate`, `business`, `dark`, `night`. Consumer / lifestyle: `cupcake`, `bumblebee`, `valentine`, `lemonade`. Bold / expressive: `synthwave`, `cyberpunk`. Default to a theme that gives a clear primary color the page can return to.
- The hero must be the visual peak: `min-h-screen` or `py-24 md:py-32`, display heading `text-6xl md:text-8xl font-bold tracking-tight`, refined subhead in `text-xl text-base-content/70 max-w-2xl`, CTAs as `btn btn-primary btn-lg` paired with `btn btn-ghost btn-lg`. Optional gradient background `bg-gradient-to-br from-primary/20 via-base-100 to-secondary/20` or full-bleed image.
- Features grid uses `card` surfaces with `shadow-xl` and inline SVG icons (24-32px) tinted via `text-primary`. Avoid bare `<div><h2>...</h2><p>...</p></div>` blocks.
- Use real visual rhythm: alternate `bg-base-100` and `bg-base-200` section backgrounds, generous `py-16 md:py-24` padding.
- Buttons must look like buttons — DaisyUI `btn` variants, never bare `<a>` with inline padding.
