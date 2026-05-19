# Product waitlist

## Purpose
A richer pre-launch page than `email-capture` — hero, feature highlights, signup form, optional social proof. Sells the product as well as collecting interest.

## What ships
- `skeleton/index.html` — single-page layout with placeholder hero / features / form.

## Checks
- `index.html` must contain `<h1`, `<form`, an email `<input type="email">`, and a submit button — enough to be a real signup page.

## Gotchas
Despite having a form, this template **does not persist submissions** — `enables_functions` is off, so there's no `/api/*` endpoint, and the prompt tells the agent to toggle an inline thank-you state via a small `<script>`. If users need real email capture, point them at the `email-capture` template instead.
