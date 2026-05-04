---
{
  "label": "Portfolio",
  "description": "Grid of projects or works with titles, descriptions, and links.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "portfolios need an <h1> with the creator's name or studio"
    }
  ]
}
---
Site type: portfolio.

- index.html showcases a creator's projects in a responsive grid.
- Sections: a brief hero (name + what you do), a grid of project cards (title, one-line description, optional tag, link), and a short contact footer.
- 4–6 project cards is a good default. Each card should be self-contained — solid color background or CSS-only thumbnail is fine; no external images.
- If the user mentions multiple projects in their prompt, create a card per project. Inline CSS only.
