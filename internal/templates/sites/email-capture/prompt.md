---
{
  "label": "Email capture",
  "description": "Single page that collects email addresses before launch.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "type=\"email\"", "type=\"submit\""],
      "message": "email capture pages need a <form> with an email input and a submit button in index.html"
    }
  ]
}
---
Site type: email capture.

- index.html exists for one job: collect an email address.
- Include a brief value proposition above the form.
- Include a `<form>` with an `<input type="email" required>` and a `<button type="submit">`.
- Show a thank-you state inline (a hidden div toggled with a tiny inline `<script>`) or link to a `thanks.html` via the form's `action`.
- Inline CSS only. Keep the visual focus on the form.
