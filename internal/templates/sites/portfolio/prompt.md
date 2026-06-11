---
{
  "label": "Portfolio",
  "description": "Showcase of projects, works, or case studies with strong visual treatment.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "portfolios need an <h1> with the creator's name or studio"
    }
  ],
  "guide": [
    {
      "id": "work",
      "label": "Your work samples",
      "why": "Work samples are the whole point of a portfolio — people want to see what you make.",
      "how": "Add a 'Work' or 'Projects' section with a few real pieces.",
      "detector": "section_present",
      "params": { "keywords": ["work", "projects", "portfolio", "selected", "case stud"] }
    },
    {
      "id": "about",
      "label": "A short about / bio",
      "why": "A couple of sentences tell people who they'd be hiring.",
      "how": "Add an 'About' section with a short bio.",
      "detector": "section_present",
      "params": { "keywords": ["about", "bio", "who i am", "who we are"] }
    },
    {
      "id": "contact",
      "label": "A contact email",
      "why": "People can't hire you if they can't reach you.",
      "how": "Add your email as a tap-to-email (mailto:) link.",
      "detector": "email_link"
    }
  ]
}
---
Site type: portfolio.

Common patterns (pick what fits the user's content):
- Hero with the creator's name + one-line statement of what they make. Bold display type.
- Project grid OR project list. Cards work well for visual work (design, photography, illustration); a vertical list with case-study links works better for writing, engineering, research.
- A short about / bio section.
- Contact / social.
- For each project the user mentions, create a card with: title, one-line description, optional tags (`badge`), and a link (even if `href="#"` because there's no destination).

Aesthetic bar: this should feel like a designer's portfolio, not a directory page.

- Pick a `data-theme` that matches the creator's medium. Visual artists: `cupcake`, `valentine`, `synthwave`, `retro`. Engineers/writers: `dark`, `night`, `coffee`, `forest`. Studios: `corporate`, `business`, `winter`.
- Hero is the chance to set tone. Big display heading (`text-6xl md:text-8xl tracking-tight`), short subhead, plenty of empty space around it. Optional decorative inline SVG (blob, geometric shape) absolute-positioned behind the heading.
- Project cards: DaisyUI `card` with `shadow-xl bg-base-100`, `hover:scale-105 transition-transform`. For visual work, the card's top is a thumbnail — either an uploaded image (`<img>` from `list_assets`) or a CSS gradient (`bg-gradient-to-br from-primary to-secondary`) with the project number/initial overlaid. Card body has title (`card-title`), one-line description, badge tags.
- Grid: `grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-6 md:gap-8` inside a `max-w-6xl mx-auto px-6` container.
- Type hierarchy must be visible: display, project title, body, caption.
- 4–6 cards is a good default; create more if the user listed more projects.
