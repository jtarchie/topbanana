---
{
  "label": "Link in bio",
  "description": "Centered profile page with photo, bio, and a stack of links — a Linktree replacement.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1", "<a "],
      "message": "link-in-bio pages need an <h1> name and at least one <a> link"
    }
  ]
}
---
Site type: link-in-bio.

Common patterns:
- Centered column, ~26rem wide, vertically padded for mobile.
- Avatar at the top (DaisyUI `avatar` component — image if uploaded, otherwise `avatar-placeholder` with the person's initial).
- Name as `<h1>` and a one-line bio underneath.
- A vertical stack of full-width link buttons (DaisyUI `btn btn-block` or custom `card` "rows"). Each link gets an icon (inline SVG) on the left and the label centered.
- Optional footer with a small "made with" or copyright line.

Aesthetic bar: this should feel modern and tactile, like a real Linktree profile.

- Pick an expressive `data-theme` by default — `cupcake`, `valentine`, `pastel`, `synthwave`, `bumblebee` all suit this layout. Fall back to `light`/`dark` only when the user asks for "minimal" or "professional".
- Background should not be flat. Use a soft gradient: `bg-gradient-to-br from-primary/20 via-base-100 to-accent/20`, or a full-bleed `bg-base-200` with an inline SVG mesh / blob behind the column.
- Link buttons must be large and tap-friendly (`btn btn-block btn-lg` or equivalent custom cards with `py-4 px-6`). Add icons on the left — inline SVG, 20-24px, tinted with the theme's accent.
- Hover state: subtle lift (`hover:-translate-y-0.5 transition-transform`) and shadow change.
- Mobile-first sizing: assume the page renders on phones. `min-h-screen flex items-center justify-center px-4 py-8` on the body, single column always.
