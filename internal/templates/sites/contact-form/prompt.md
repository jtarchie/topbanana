---
{
  "label": "Contact form",
  "description": "One-page site with a working form that posts to /api/submit. Server-side handler included.",
  "enables_functions": true,
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "/api/submit"],
      "message": "contact-form sites need a <form> that posts to /api/submit"
    }
  ],
  "guide": [
    {
      "id": "form",
      "label": "A form on your home page",
      "why": "The form is the reason this site exists.",
      "how": "Add a form to your home page so visitors can submit their details.",
      "detector": "form",
      "scope": "specific-file",
      "page": "index.html"
    },
    {
      "id": "intro",
      "label": "A line explaining what they're signing up for",
      "why": "One sentence above the form tells visitors what they're submitting and raises completion.",
      "how": "Add a short heading and sentence describing the form above it.",
      "detector": "section_present",
      "params": { "keywords": ["sign up", "contact", "get in touch", "rsvp", "preorder", "join"] },
      "required": false
    }
  ]
}
---
Site type: contact form / preorder / RSVP.

- index.html is a single-page site with a clear <h1>, a one-line description of what the visitor is signing up for, and an HTML form that POSTs to `/api/submit`.
- Form fields belong inside the form. Use the visitor's request to decide fields (e.g. a hot-dog preorder might want `name`, `count`, `pickup`; an RSVP might want `name`, `attending`, `plus_one`).
- thanks.html is the post-submit confirmation page. Keep it short and warm.
- functions/submit.js handles the POST. It must read `request.form` (form-encoded body is pre-parsed), log the submission, and `return response.redirect("/thanks.html")`. Persistence will be added in a later iteration; for now, logging is enough.
- Inline CSS only. Keep the form visually obvious — large input fields, clear submit button.
