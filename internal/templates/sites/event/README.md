# Event invite

## Purpose
A generic invitation — meetup, wedding, conference, dinner. The agent fills in event title, date/time, location, description, and RSVP method (mailto, phone, or simple form) based on the user's prompt.

## What ships
- `skeleton/index.html` — placeholder layout with the fields the agent should replace.

## Checks
- `index.html` must contain `<h1` — every invite needs a clear event title.

## Completeness guide
Owner-facing essentials on the manage page (detector in parens): Date and time (`section_present`) · Where it's happening (`section_present`) · A way to RSVP (`form`) · A short description (`section_present`, optional). The RSVP item detects a `<form>`; the prompt also allows mailto/phone RSVP, so it's the credible default rather than the only acceptable answer.
