# Email capture

## Purpose
A single-purpose page that collects email addresses pre-launch. Submissions persist to the kv store and are visible to the owner from the manage page's submissions table and via the CSV/JSON download.

## What ships
- `skeleton/index.html` — minimal page with `<form action="/api/submit">` and an email input.
- `skeleton/thanks.html` — post-submit confirmation page.
- `skeleton/functions/submit.js` — handler that increments `submission_seq`, persists `submission:NNNNNNNN`, and redirects to `/thanks.html`.

## Checks
- `index.html` must contain `<form`, `type="email"`, `type="submit"`, and `/api/submit` — the form must actually be functional.

## Config
- `enables_functions: true` — required for the `/api/submit` endpoint and kv access.
- `setup_notes` tells end users where captured emails surface and how to export them.

## Gotchas
There's no double opt-in step; the form takes the address and stores it. Make sure the page copy is clear about what the visitor is signing up for, since you can't revisit consent after the fact.
