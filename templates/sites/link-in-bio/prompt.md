---
{
  "label": "Link in bio",
  "description": "Centered profile page with a photo, short bio, and a stack of links.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1", "<a "],
      "message": "link-in-bio pages need an <h1> name and at least one <a> link button"
    }
  ]
}
---
Site type: link in bio.

- index.html is a single centered page styled like a Linktree replacement.
- Include: a circular avatar (a CSS-only initial bubble is fine), the person's name as `<h1>`, a one-line bio, and a vertical stack of full-width link buttons.
- Each link should be a large, easily tappable `<a>` styled as a button. Mobile-first sizing.
- Inline CSS only. Use a soft gradient or solid pastel background.
