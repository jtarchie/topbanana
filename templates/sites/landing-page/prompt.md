---
{
  "label": "Landing page",
  "description": "Marketing page with a hero, value prop, features, and a call-to-action.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "landing pages need a clear <h1> headline in index.html"
    }
  ]
}
---
Site type: landing page.

- index.html is a single-page marketing site.
- Include: a clear hero `<h1>` headline, a one-line value proposition, a primary call-to-action button or link, and a short features or benefits section (3 items is plenty).
- Visual hierarchy matters. Use generous spacing, large headings, and obvious section breaks. Inline CSS only.
- Keep copy specific to whatever the user asked for — replace every placeholder.
