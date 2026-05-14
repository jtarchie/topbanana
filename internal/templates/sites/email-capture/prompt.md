---
{
  "label": "Email capture",
  "description": "Single page that collects email addresses before launch. Submissions persist server-side and are visible to the owner in /apps.",
  "enables_functions": true,
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "type=\"email\"", "type=\"submit\"", "/api/submit"],
      "message": "email capture pages need a <form> that POSTs to /api/submit with an email input and a submit button in index.html"
    }
  ]
}
---
Site type: email capture.

- index.html exists for one job: collect an email address.
- Include a brief value proposition above the form.
- Include a `<form action="/api/submit" method="post">` with an `<input type="email" name="email" required>` and a `<button type="submit">`. The `name="email"` matters — the server-side handler reads `request.form.email`.
- Show the post-submit confirmation on `thanks.html` (the handler redirects there). Keep it short and warm.
- functions/submit.js handles the POST. It must read `request.form`, persist the email with a monotonic key (`kv.incr("submission_seq")` then `kv.put("submission:" + padded, { email, ts })`), and `return response.redirect("/thanks.html")`. Owners view captured emails from the admin /apps page.
- Inline CSS only. Keep the visual focus on the form.
