---
{
  "label": "Restaurant / menu",
  "description": "Local-business page: name, hours, menu sections, location, and contact.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "restaurant pages need a clear <h1> with the restaurant name"
    }
  ]
}
---
Site type: restaurant / local business.

- index.html is the front door for a small restaurant or café.
- Sections in order: name + tagline hero, Hours, Menu (grouped into Starters / Mains / Desserts or whatever fits — each item has a name, short description, and price), Location/contact.
- Warm, appetizing palette. Serif headings work well. Inline CSS only.
- If the user describes a real cuisine, replace the placeholder menu items with thematically appropriate dishes.
