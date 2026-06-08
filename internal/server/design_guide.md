# Design substrate

Every page links **one** stylesheet — the platform compiles and self-hosts it
per site:

```html
<link rel="stylesheet" href="/app.css">
```

There are **no** CDN tags. `/app.css` is built from the page's own markup
after you run `lint_site`, so a freshly written page is unstyled until you
lint it.

## Vocabulary
- **Tailwind utility classes** for layout/spacing/typography: `flex`, `grid`, `gap-4`, `p-6`, `text-lg`, `font-bold`, `max-w-3xl`, `mx-auto`.
- **daisyUI component classes** for ready-made UI: `btn`, `btn-primary`, `card`, `navbar`, `hero`, `badge`, `alert`, `menu`, `modal`, `table`.

## Themes
Set the palette on the root element; daisyUI ships every theme, so switching is
just an attribute — no recompile:

```html
<html data-theme="corporate">   <!-- or: dark, emerald, synthwave, retro, ... -->
```

## Rules
- Inline any JS in a `<script>` tag — no external scripts or frameworks.
- Relative links between pages (`<a href="about.html">`); `index.html` is the entry point.
- Keep each page self-contained.