# Contact form

## Purpose
A single-page site with a working form that POSTs to `/api/submit` and redirects to a thank-you page. Picked for contact pages, RSVPs, preorders — anywhere a visitor needs to send a small structured payload.

## What ships
- `skeleton/index.html` — page with `<form action="/api/submit">`.
- `skeleton/thanks.html` — post-submit confirmation page.
- `skeleton/functions/submit.js` — server-side handler that reads `request.form` and redirects to `/thanks.html`.

## Checks
- `index.html` must contain `<form` and `/api/submit` — the form's destination is load-bearing.

## Completeness guide
Owner-facing essentials on the manage page (detector in parens): A form on your home page (`form`, scope `specific-file` index.html) · A line explaining what they're signing up for (`section_present`, optional).

## Config
- `enables_functions: true` — this template opts into the `/api/*` router and the function-editing tools.

## Gotchas
The current skeleton's `submit.js` only logs the submission — the prompt says "persistence will be added in a later iteration." If the user expects to see submissions later, the agent (or you) needs to swap in `kv.put(...)` calls similar to `email-capture` or `guestbook`.
