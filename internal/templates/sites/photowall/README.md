# Event photo wall

## Purpose
A moderated, visitor-facing photo wall for events. Guests scan a QR code, land on the upload page, and submit a photo; every photo is held for the host's approval; approved photos rotate on a full-screen display that refreshes itself. Picked when someone wants a live, curated photo feed at a party, wedding, conference, or meetup.

## What ships
- `skeleton/index.html` — the upload page (the QR target). A `<form method="POST" action="/_photos" enctype="multipart/form-data">` with a single `photo` file input, plus a thank-you banner shown after the `/?submitted=1` redirect.
- `skeleton/display.html` — the full-screen wall. Inline JS polls `/_photos/approved` every few seconds and cross-fades through the approved photos; no page reload during the event.

## Checks
- `index.html` must contain `<form`, `/_photos`, and `type="file"` — the upload form is the whole point.
- `display.html` must contain `/_photos/approved` — without the poll the wall never fills.

## Completeness guide
Owner-facing essentials on the manage page (detector in parens): A photo upload form (`form`, scope `specific-file` index.html) · A line inviting guests to scan and upload (`section_present`, optional).

## Config
- `enables_photo_wall: true` — opts the site into the platform's photo endpoints (`POST /_photos`, `GET /_photos/approved`) and the owner-facing moderation queue (linked from the Photo wall card on `/manage/:slug`). This template does **not** enable functions; there is no `/api/` handler.

## Gotchas
- Uploads and the approved list are served by the platform (Go), not by static files or `/api/` functions — the upload endpoint accepts multipart (which `/api/` cannot). Don't write a `functions/upload.js`; post the form straight to `/_photos`.
- Un-approved photos are stored under a reserved prefix the public site never serves, so a pending photo can't be viewed until it's approved. Approval copies the bytes into the public `assets/photowall/` tree.
- The open upload link is rate-limited per visitor and the pending queue is capped; when the cap is hit, uploads are refused until the host clears the backlog.
- The `photo` field name is load-bearing — the server reads `photo` from the multipart form. Renaming it breaks uploads.
