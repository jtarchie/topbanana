---
{
  "label": "Resume / personal site",
  "description": "One-page personal résumé and brand site with about, experience, skills, and contact.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "résumé pages need a clear <h1> with the person's name"
    }
  ],
  "guide": [
    {
      "id": "experience",
      "label": "Your experience",
      "why": "Experience is what most readers scan first.",
      "how": "Add an 'Experience' section listing roles, places, and dates.",
      "detector": "section_present",
      "params": { "keywords": ["experience", "work history", "employment"] }
    },
    {
      "id": "skills",
      "label": "Your skills",
      "why": "A skills list helps recruiters match you quickly.",
      "how": "Add a 'Skills' section.",
      "detector": "section_present",
      "params": { "keywords": ["skills", "expertise", "tools", "technologies"] },
      "required": false
    },
    {
      "id": "contact",
      "label": "A contact email",
      "why": "Make it one click for someone to reach you.",
      "how": "Add your email as a tap-to-email (mailto:) link.",
      "detector": "email_link"
    }
  ]
}
---
Site type: personal résumé / professional brand site.

Common patterns (pick what fits the user's content; order to taste — there is no "correct" order):
- Hero with the person's name, role/tagline, optional avatar or photo. If the user uploaded an image, use it (DaisyUI `avatar` component for a round profile shot, or as a full-bleed hero background).
- About / summary — a paragraph or two on who they are.
- Experience — present as DaisyUI `timeline` (chronological story) OR `card` grid (skim-friendly), whichever suits the content density.
- Skills — DaisyUI `badge` cluster with `flex flex-wrap gap-2`.
- Selected projects, education, awards, speaking — only if the user mentioned them.
- Contact — email, social links. The DaisyUI `btn` component works well for contact CTAs.

Aesthetic bar: this should look like a personal brand site, not an academic CV.

- Pick a `data-theme` that fits the user's vibe. Default to `light` or `dark` for professional/corporate roles; reach for `cupcake`, `bumblebee`, `valentine`, `lemonade`, or `coffee` for creative roles; `synthwave`, `cyberpunk`, or `retro` for designers/engineers who want personality.
- The hero must do real work — large display headline (`text-5xl md:text-7xl font-bold tracking-tight`), refined subhead, optional gradient or image background. Not a thin underlined name bar.
- Type hierarchy must be visible at a glance: display heading, section headings (`text-3xl font-bold`), body, captions.
- Experience entries belong on `card` surfaces with `shadow-xl` and `bg-base-100`, or in a `timeline`. Flat bullet lists under a heading look dated.
- Generous whitespace: `py-16` to `py-24` between major sections, `max-w-4xl mx-auto` for content rails.
- Accent color comes from the theme (`text-primary`, `bg-accent`), never hardcoded hex.
