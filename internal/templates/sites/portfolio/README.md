# Portfolio

## Purpose
A creator's project showcase — hero, project grid or list, about, contact. Picked when the user wants to display visual or written work (design, photography, illustration, writing, engineering).

## What ships
- `skeleton/index.html` — placeholder layout that the agent rewrites against the user's projects.

## Checks
- `index.html` must contain `<h1` — every portfolio needs a name or studio in the headline.

## Completeness guide
Owner-facing essentials on the manage page (detector in parens): Your work samples (`section_present`) · A short about / bio (`section_present`) · A contact email (`email_link`). Uses `section_present` for work (not `min_images`) because the prompt allows CSS-gradient thumbnails instead of `<img>`.

## Gotchas
Like `landing-page` and `resume`, the prompt carries strong aesthetic guidance (theme selection by medium, card hover effects, grid sizing). Without it the agent produces a flat list, not a portfolio.
