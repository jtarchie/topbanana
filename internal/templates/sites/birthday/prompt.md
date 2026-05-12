---
{
  "label": "Birthday announcement",
  "description": "Festive single-page announcement for a birthday or party.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "birthday pages need a big <h1> with the celebrant's name or party title"
    }
  ]
}
---
Site type: birthday announcement.

- index.html announces a birthday or birthday party.
- Use playful, festive styling — bright colors, generous spacing, large display fonts. Inline CSS only.
- Include the celebrant's name (or the party's name), the date and time, the location, and an RSVP method (a `mailto:` link or a phone number is fine).
- Replace every placeholder with whatever the user described.
