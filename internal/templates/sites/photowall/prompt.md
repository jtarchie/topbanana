---
{
  "label": "Event photo wall",
  "description": "Guests scan a QR code, upload a photo, and approved photos rotate on a full-screen display. Every photo waits for the host's approval.",
  "enables_photo_wall": true,
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "/_photos", "type=\"file\""],
      "message": "the upload page needs a <form> with a file input that posts to /_photos (multipart)"
    },
    {
      "file": "display.html",
      "must_contain": ["/_photos/approved"],
      "message": "the display page must poll /_photos/approved to load approved photos"
    }
  ],
  "guide": [
    {
      "id": "upload_form",
      "label": "A photo upload form",
      "why": "The upload form is the whole point — it's how guests add photos.",
      "how": "Keep a <form> on the home page with a file input that posts to /_photos.",
      "detector": "form",
      "scope": "specific-file",
      "page": "index.html"
    },
    {
      "id": "scan_prompt",
      "label": "A line inviting guests to scan and upload",
      "why": "Guests arriving from a QR code need a clear prompt telling them what to do.",
      "how": "Add a short heading or sentence like \"Scan and share your photo\" above the form.",
      "detector": "section_present",
      "params": { "keywords": ["scan", "qr", "upload", "share your photo"] },
      "required": false
    }
  ],
  "setup_notes": "Two pages ship with this site. display.html is the big-screen wall — open it full-screen on the projector or TV. It shows a scannable QR code in the corner (pointing guests to the upload page) and checks for newly-approved photos every few seconds, so nothing needs reloading during the event.\n\nindex.html is the upload page guests reach by scanning that QR (its URL is your site address). \n\nEvery uploaded photo waits for you: open the moderation queue (the Photo wall card on this page) and tap Approve to send a photo to the display, or Reject to discard it."
}
---
Site type: event photo wall — guests upload photos, the host approves them, approved photos rotate on a full-screen display.

- index.html is the upload page guests reach by scanning a QR code. It has a clear heading, a one-line invitation to scan and share a photo, and an HTML form that posts a single photo:
  `<form method="POST" action="/_photos" enctype="multipart/form-data"><input type="file" name="photo" accept="image/*" required> ...</form>`.
  The field MUST be named `photo`. After a successful upload the server redirects back to `/?submitted=1` — show a friendly "thanks, it'll appear once approved" confirmation when that query flag is present.
- Do NOT add name or caption fields — v1 collects the photo only.
- display.html is the full-screen wall. Inline JavaScript fetches `/_photos/approved` every few seconds (it returns a JSON array of `{url, ts}`, newest first) and rotates through the photos with a gentle cross-fade. It never needs a full page reload. Style it for a dark room: full-bleed images on a black background.
  - Keep the QR-code corner: an `<img src="/_photos/qr">` (the platform renders a scannable SVG QR of this site's upload page) with a short "Scan to add your photo" label, pinned to a corner so guests can scan the screen and upload. Do not fetch a QR from any external service — `/_photos/qr` is served by the platform.
- There is NO /api/ handler and no functions/ for this template — uploads and the approved list are served by the platform at /_photos and /_photos/approved. Do not write functions.
- Inline CSS/JS only, self-contained, `/app.css` on every page. Relative links between pages.
