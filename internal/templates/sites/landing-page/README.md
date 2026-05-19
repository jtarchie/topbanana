# Landing page

## Purpose
A marketing page — sticky navbar, hero with primary/secondary CTAs, social proof, features grid, optional pricing/FAQ, footer CTA. Picked when the user wants to "sell" something to visitors (a SaaS product, an app, a service).

## What ships
- `skeleton/index.html` — sectioned layout the agent rewrites against the user's product description.

## Checks
- `index.html` must contain `<h1` — every landing page needs a clear hero headline.

## Gotchas
The prompt addendum carries strong aesthetic direction (theme selection by product tone, hero must be `min-h-screen` or `py-24+`, etc.). Without it the agent produces a brochure, not a marketing page. If you're modifying the prompt, keep the "hero must be the visual peak" framing.
