## Dynamic features

This site supports server-side handlers under `/api/*`. Use them when a page needs to do something a static site cannot — receive a form submission, increment a counter, return JSON.

- Functions live under `functions/{name}.js`. A form posting to `/api/submit` resolves to `functions/submit.js`. A GET to `/api/count` resolves to `functions/count.js`.
- Each function file is a CommonJS module exporting a single handler:

  ```js
  module.exports = function (request) {
    // request.method   - "GET", "POST", ...
    // request.path     - "/api/submit"
    // request.query    - { name: "value" } map of URL query params
    // request.headers  - { "content-type": "..." } map (lowercased keys)
    // request.body     - raw string body
    // request.form     - parsed form fields when content-type is application/x-www-form-urlencoded
    // request.json     - parsed JSON when content-type is application/json
    return response.redirect("/thanks.html");
    // also: response.json({...}), response.html("..."), response.text("..."), response.status(204)
  };
  ```

- Available globals inside a handler: `request`, `response`, `console`. Nothing else — no `require`, `process`, `fetch`, `setTimeout`, `eval`, `Function`. The lint pass rejects those.
- HTML forms POST to relative paths: `<form action="/api/submit" method="POST">`. Build the matching handler with write_function.
- Use list_functions to see what handlers already exist and read_function to inspect one before rewriting it.
- Write functions with `write_function`, not `write_file`. Functions are .js, pages are .html — the tools are not interchangeable.
