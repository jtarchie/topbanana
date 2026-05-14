## Dynamic features

This site supports server-side handlers under `/api/*`. Use them when a page needs to do something a static site cannot — receive a form submission, increment a counter, render data, return JSON.

### File layout
- Functions live under `functions/<name>.js` (where `<name>` is the handler name). A form posting to `/api/submit` resolves to `functions/submit.js`. A GET to `/api/count` resolves to `functions/count.js`. The name must match `[a-z0-9_-]{1,40}`.
- Each function file is a CommonJS module exporting a single handler: `module.exports = function (request) { ... }`. The handler receives one argument and returns a response.
- Write functions with `write_function`, never `write_file`. Pages are `.html`, handlers are `.js` — the tools are not interchangeable.
- Use `list_functions` before writing to see what exists, and `read_function` to inspect a handler before rewriting it.
- For surgical edits to an existing handler, prefer `edit_function` over `write_function`: same exact-match `old_text`/`new_text` semantics as `edit_file`. Re-emit the whole file only when the change is larger than the unchanged portion.
- Remove a handler with `delete_function` when it's no longer reachable from any page. The `/api/<name>` route 404s after deletion. There is no `delete_file` for HTML — leave stale HTML pages in place or rewrite them with `write_file`.

### Handler contract

```js
module.exports = function (request) {
  // ...your logic here
  return response.json({ ok: true });
};
```

The handler must return a value built from a `response.*` builder (below) or a plain object of shape `{ status, contentType, body, headers }`. Returning nothing produces a 204.

### `request`

| field | type | notes |
|---|---|---|
| `request.method` | string | `"GET"`, `"POST"`, ... |
| `request.path` | string | e.g. `"/api/submit"` |
| `request.query` | object | parsed URL query, string values |
| `request.headers` | object | header map with lowercase keys |
| `request.body` | string | raw request body, always present |
| `request.form` | object | pre-parsed when `Content-Type: application/x-www-form-urlencoded` |
| `request.json` | any | pre-parsed when `Content-Type: application/json` |

Form posts from HTML forms arrive in `request.form`; you almost never need `request.body` for HTML.

### `response`

Each builder returns a response object the host serializes back. Pick the one that matches what you're sending.

```js
response.json({ ok: true });             // 200, application/json
response.html("<h1>Hello</h1>");         // 200, text/html
response.text("ok");                     // 200, text/plain
response.redirect("/thanks.html");       // 303, sets Location
response.redirect("/login", 302);        // 302, sets Location
response.status(204);                    // 204, no body
response.status(400, "name required");   // 400, text/plain body
```

### `kv` — per-site key-value state

Functions can read and write a per-site key-value store. State is persistent across requests and across server restarts. Values must be JSON-serializable (string, number, boolean, array, plain object, `null`); Dates and class instances are rejected at `put`-time — convert with `.toISOString()` or `Date.now()` first.

```js
kv.get(key, defaultValue);   // returns value, or defaultValue, or null
kv.put(key, value);          // store value; mutation persists after the handler returns
kv.delete(key);              // remove key (no-op if missing)
kv.incr(key, delta);         // delta defaults to 1; returns the new integer value
kv.list(prefix);             // returns [{key, value}] sorted by key; empty prefix lists everything
```

Important behaviours to know:

- **`kv.list` always returns rows sorted by key.** Zero-pad numeric suffixes so insertion order matches lexicographic order: `"order:" + String(n).padStart(8, "0")` gives keys that sort the way they were created.
- **`kv.incr` creates the key if missing**, starting from `delta`. It throws if the existing value isn't numeric.
- **`kv` is scoped to the current site automatically.** You cannot reach another site's data — there is no `slug` argument, deliberately.

### `console`

`console.log`, `console.info`, `console.warn`, `console.error`, `console.debug` all route to the server log and the live editor SSE stream. Use them for debugging; do not rely on them for user-facing output.

### Available globals

`request`, `response`, `console`, `kv`. **Nothing else.** No `require`, `process`, `fetch`, `setTimeout`, `eval`, `Function`, `globalThis`, `WebAssembly`. The lint pass rejects those before the handler ever runs — if you reach for one, the build will fail with a clear error.

### Common patterns

**Form submission that persists (`functions/submit.js`):**

```js
module.exports = function (request) {
  var n = kv.incr("submission_seq");
  kv.put("submission:" + String(n).padStart(8, "0"), request.form || {});
  return response.redirect("/thanks.html");
};
```

**JSON counter for a page to fetch on load (`functions/count.js`):**

```js
module.exports = function () {
  return response.json({ count: kv.get("submission_seq", 0) });
};
```

**List endpoint that returns all entries in insertion order (`functions/list.js`):**

```js
module.exports = function () {
  return response.json(kv.list("entry:").map(function (r) { return r.value; }));
};
```

HTML forms POST to relative paths: `<form action="/api/submit" method="POST">`. Build the matching handler with `write_function`.
