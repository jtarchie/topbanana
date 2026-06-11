# Restaurant / menu

## Purpose
Front-door page for a small restaurant or café — name + tagline, hours, menu sections, location, contact. Picked when the user describes a local food business.

## What ships
- `skeleton/index.html` — sectioned layout with placeholder menu items.

## Checks
- `index.html` must contain `<h1` — every restaurant page needs a clear name in the headline.

## Completeness guide
Owner-facing essentials on the manage page (detector in parens): Opening hours (`section_present`) · Your menu (`section_present`) · Your location (`address`) · A tap-to-call phone number (`tel_link`) · A map link (`map_link`, optional).

## Gotchas
The prompt nudges the agent toward "warm, appetizing palette" and serif headings. If you're rewriting the prompt for a different cuisine vibe, keep that aesthetic guidance — without it the agent defaults to corporate.
