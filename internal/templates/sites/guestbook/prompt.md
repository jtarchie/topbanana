---
{
  "label": "Guestbook",
  "description": "Visitors leave a message and see the wall of past messages. State persists across restarts.",
  "enables_functions": true,
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "/api/sign"],
      "message": "guestbook sites need a <form> that posts to /api/sign"
    }
  ]
}
---
Site type: guestbook / shared wall / public message board.

- index.html is a single-page site with a clear <h1>, a one-line description, an HTML form that POSTs to `/api/sign`, and a section that renders all signed messages.
- functions/sign.js stores the submission with a monotonic key (`kv.incr("seq")` then `kv.put("entry:" + seq, { name, message, ts })`) and redirects back to `/`.
- functions/list.js returns all entries as JSON (use `kv.list("entry:")`). The page fetches `/api/list` on load and renders the entries newest-first.
- Inline CSS only. The wall should feel warm — messages stacked, name in bold, timestamp muted.
