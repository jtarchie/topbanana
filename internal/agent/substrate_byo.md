## Styling: this site has its own stylesheet

This site does not use the platform's component library. It carries its own hand-authored stylesheet — %s — and that file, not your markup, decides how every page looks. It has been pre-loaded into your conversation history via read_file. Read it before you change anything visual.

**Style changes belong in the stylesheet.** Use `edit_file` on it exactly as you would on a page. This is the single most important rule here, because of how the cascade works:

- Everything in `/app.css` is inside a CSS `@layer`. This site's stylesheet is not. Unlayered rules beat layered ones **regardless of specificity or link order**, so a rule in this site's stylesheet defeats every Tailwind and DaisyUI class, always. Adding classes to markup to change an element the stylesheet already styles does nothing at all.
- `width` and `height` attributes on `<img>` are presentation *hints*. Any stylesheet rule outranks them. If the stylesheet sets a size on that image, changing the attribute changes nothing on screen.
- So: when asked to resize, respace, recolour or re-lay-out something, find the rule that currently controls it and change that rule. Grep the stylesheet for the element's class before you touch the HTML.

**Match the design language that is already here.** This site has a considered visual identity and your job is to extend it, not to replace it:

- Reuse its custom properties (the `--name` values defined at the top) rather than introducing new colours. Never hard-code a hex value the stylesheet doesn't already use.
- Reuse its existing class names and component patterns. New markup should look like the markup already on the page.
- Add new rules in the stylesheet next to related ones, in its established formatting style.
- Respect its responsive behaviour: it has its own media queries, so a size change usually needs updating the matching rule inside them too.
- Do **not** introduce Tailwind utility or DaisyUI component classes (`btn`, `card`, `hero`, `text-primary`, `grid-cols-3`, …). They do not belong to this site's design language, and the cascade means they would lose to its own rules anyway. `search_docs` searches the DaisyUI reference and is not useful here.
- Leave the `/app.css` link in `<head>` alone. The platform maintains it; it simply is not what styles this site.

**Everything else still holds:**

- Do not create a second stylesheet. Edit the one that exists.
- Keep the page responsive and never horizontally scrolling: constrain long prose, wrap wide tables and `<pre>` blocks so they scroll inside their own container, and let long unbreakable strings wrap.
- Keep a real type hierarchy and generous whitespace between sections.
- Icons and decorative accents are inline SVG, never Unicode emoji — emoji render inconsistently across devices, browsers and themes and read as unpolished. Colour them with `fill="currentColor"` / `stroke="currentColor"` so they pick up the surrounding text colour, and mark purely decorative ones `aria-hidden="true"`. For repeated icons, define a `<symbol id="…">` sprite once and reference it with `<use href="#…">` (every `#id` must resolve to a symbol on the same page).
- Preserve the accessibility already in the markup: alt text, `aria-hidden` on decorative elements, visible focus styles, and label/for pairings.
