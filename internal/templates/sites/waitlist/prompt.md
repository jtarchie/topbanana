---
{
  "label": "Product waitlist",
  "description": "Pre-launch product page: hero, features, social proof, and an email signup.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1", "<form", "type=\"email\"", "type=\"submit\""],
      "message": "waitlist pages need an <h1>, plus a <form> with an email input and a submit button"
    }
  ]
}
---
Site type: product waitlist / pre-launch.

- index.html is a richer pre-launch page than a bare email-capture: it sells the product as well as collecting addresses.
- Sections in order: hero (`<h1>` product name + value prop), 3 feature highlights, signup form, optional social-proof line.
- The signup form must contain `<input type="email" required>` and `<button type="submit">`.
- Show an inline thank-you state (toggle visibility with a tiny inline `<script>`).
- Inline CSS only. Modern, confident, slightly opinionated styling.
