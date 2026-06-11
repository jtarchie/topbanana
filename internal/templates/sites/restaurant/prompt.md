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
  ],
  "guide": [
    {
      "id": "hours",
      "label": "Opening hours",
      "why": "Visitors check your hours before deciding to come.",
      "how": "Add an 'Hours' section with the days and times you're open.",
      "detector": "section_present",
      "params": { "keywords": ["hours", "open", "opening", "closed"] }
    },
    {
      "id": "menu",
      "label": "Your menu",
      "why": "The menu is the #1 thing people look for on a restaurant site.",
      "how": "Add a 'Menu' section with item names, short descriptions, and prices.",
      "detector": "section_present",
      "params": { "keywords": ["menu", "starters", "mains", "dishes", "specials"] }
    },
    {
      "id": "location",
      "label": "Your location",
      "why": "People need to know where to find you.",
      "how": "Add your address — an address block or a 'Location' section.",
      "detector": "address"
    },
    {
      "id": "phone",
      "label": "A tap-to-call phone number",
      "why": "A tappable number lets mobile visitors call in one tap to book or ask.",
      "how": "Add your phone number as a tap-to-call (tel:) link.",
      "detector": "tel_link"
    },
    {
      "id": "map",
      "label": "A map link",
      "why": "A map makes directions effortless.",
      "how": "Link or embed a Google or Apple map of your address.",
      "detector": "map_link",
      "required": false
    }
  ]
}
---
Site type: restaurant / local business.

- index.html is the front door for a small restaurant or café.
- Sections in order: name + tagline hero, Hours, Menu (grouped into Starters / Mains / Desserts or whatever fits — each item has a name, short description, and price), Location/contact.
- Warm, appetizing palette. Serif headings work well. Inline CSS only.
- If the user describes a real cuisine, replace the placeholder menu items with thematically appropriate dishes.
