# Guestbook

## Purpose
A public message wall — visitors sign with name + message, see the wall of past messages. State persists across restarts. Picked when the user wants a shared, public artifact (party guestbook, memorial wall, leave-a-message page).

## What ships
- `skeleton/index.html` — page with `<form action="/api/sign">` and a JS-fetched list of entries.
- `skeleton/functions/sign.js` — handler that increments `seq`, persists `entry:N`, and redirects.
- `skeleton/functions/list.js` — handler that returns all entries via `kv.list("entry:")` for the client to render.

## Checks
- `index.html` must contain `<form` and `/api/sign` — the form is the whole point of the template.

## Config
- `enables_functions: true` — required for `/api/sign` and `/api/list`.
- `setup_notes` warns end users that there is no moderation queue and tells them how to remove abusive entries.

## Gotchas
Entries are public by design — everyone visiting the site sees the full wall. There's no moderation, no spam filter, no rate limit beyond the sandbox's per-request defaults. If you need any of those, the owner has to bolt them on manually.
