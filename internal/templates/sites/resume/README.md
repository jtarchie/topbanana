# Resume / personal site

## Purpose
A one-page personal résumé and brand site — hero, about, experience, skills, projects, contact. Picked when the user wants a professional-but-styled presence (not a stock-CV PDF clone).

## What ships
- `skeleton/index.html` — placeholder layout that the agent rewrites against the user's bio.

## Checks
- `index.html` must contain `<h1` — every résumé needs a clear name in the headline.

## Gotchas
The prompt addendum is long and aesthetic-heavy on purpose — without the "must look like a personal brand site, not an academic CV" framing the agent regresses to flat bullet lists. If you trim the prompt, keep the type-hierarchy and DaisyUI-component guidance.
