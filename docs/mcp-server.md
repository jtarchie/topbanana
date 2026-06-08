# MCP server

Top Banana exposes a [Model Context Protocol](https://modelcontextprotocol.io)
endpoint so an MCP client (e.g. Claude Code) can **edit the sites a logged-in
user already owns**. Sites are *created* in the web `/build` flow; the MCP
surface is the *editing* interface — discover a site, inspect it, change its
pages/assets/functions/settings, and lint it. Unlike `/build`, the MCP tools
never invoke the server-side LLM build agent: the connecting agent (itself a
capable model) does the authoring directly. Every tool is deterministic (S3
reads/writes, metadata, the sandbox, and the pure-CPU lint pass).

The surface is designed around that external agent rather than mirroring the
in-process build agent's primitives: it carries the authoring conventions
through MCP **Resources** and **Prompts** (not just tool descriptions), offers
**surgical edit tools** so iterating on a page isn't a whole-file rewrite, and
returns the **live URL + a lint nudge** so the publish step is obvious. The
edit semantics are shared with the build agent via `internal/textedit`, so
MCP-driven and web-driven edits behave identically.

## Enabling it

MCP is **opt-in**. Set the signing secret:

```
MCP_SECRET=<a long random string>   # or --mcp-secret
```

When `MCP_SECRET` is empty the `/mcp` and `/oauth/*` routes are not mounted.
Keep the secret stable (rotating it invalidates outstanding bearer tokens) and
private (it signs every token).

## Connecting from Claude Code

```
claude mcp add --transport http topbanana https://<your-domain>/mcp
```

Claude Code discovers the OAuth authorization server from
`/.well-known/oauth-protected-resource`, registers itself, and opens a browser
to `/oauth/authorize`. Authentication reuses your existing **passkey login**:

- If you're already signed in to Top Banana in that browser, a bearer token is
  issued immediately.
- If not, you're bounced to `/login`; sign in, then re-run the connect so your
  session cookie satisfies the authorize step.

Tokens carry your email as the subject (`aud=mcp`), and every tool is scoped to
the sites you own (super admins see all). They expire after 12h; Claude Code
re-runs the OAuth flow to refresh.

## Tools

Discovery & files:

| Tool | Purpose |
| --- | --- |
| `list_sites` | List the sites you own (title, template, created, custom domains, URL). |
| `get_site` | Metadata (including any custom domains) + file list for one site. |
| `read_file` | Read a file from a site. |
| `write_file` | Create/overwrite a **text** file — HTML pages and text assets like `favicon.svg` (content type inferred from the extension). Returns the page URL + a lint nudge. |
| `create_upload_ticket` | Get a short-lived URL to `curl` a **binary** image (png/jpg/gif/webp) to; see below. |
| `list_files` | List file paths in a site. |
| `delete_file` | Delete a page or asset. |

Surgical editing (HTML pages; shared semantics with the build agent):

| Tool | Purpose |
| --- | --- |
| `edit_file` | Find/replace `old_text` → `new_text` (byte-exact, with a whitespace-tolerant fallback; unique unless `replace_all`). Prefer over rewriting a page. |
| `replace_lines` | Replace lines `start_line..end_line` (1-indexed, inclusive). Empty `new_text` deletes. |
| `insert_at_line` | Insert after a line without replacing (`0` prepends, `total_lines` appends). |
| `grep_files` | Literal substring search across a site's HTML + function source (paths, line numbers, snippets). |

Server-side functions (sites with functions enabled):

| Tool | Purpose |
| --- | --- |
| `write_function` / `read_function` / `edit_function` / `delete_function` / `list_functions` | Author the `functions/<name>.js` handlers served at `/api/<name>`. |
| `test_function` | Invoke a handler in the sandbox against the site's real KV state; returns status, headers, body, and console logs. |

Settings, lint & data:

| Tool | Purpose |
| --- | --- |
| `configure_site` | Update `title` / `description` / `private` / `enable_functions` / `enable_public_api` (only the fields you pass). |
| `lint_site` | Compile + self-host `/app.css` and run the deterministic checks; returns structured `problems` (`file`/`message`/`kind`/`autofixable`). This is what **publishes** the stylesheet. |
| `list_submissions` | Read a site's captured form/KV entries (newest-first, capped). |
| `list_runs` / `get_run_transcript` | Read-only transcripts of prior web-UI builds/edits. |

> Site **creation**, deletion, ownership transfer, and custom-domain management
> stay in the web UI — the MCP surface only edits sites that already exist.

### Binary uploads (upload ticket)

Binary images can't go through `write_file` — base64 would inflate a 5 MiB
image to millions of tokens and land the base64 *text* in the bucket. Instead:

1. `create_upload_ticket(slug, filename?)` returns a short-lived URL on the app's
   own domain plus a copy-paste `curl` recipe.
2. The agent `curl -X POST --data-binary @file "<upload_url>?filename=logo.png"`.
3. The server verifies the signed ticket, caps the size, **sniffs and
   allowlists** the real content type (`{jpeg,png,gif,webp,svg}` — a declared
   type is never trusted), stores it under `assets/`, and best-effort captions
   it. The response gives the authoritative `assets/<name>` path to drop into
   `<img src="…">`.

The ticket is an HS256 JWT signed with `MCP_SECRET` but pinned to the
`upload-ticket` audience (so it's not interchangeable with an MCP bearer token),
scoped to one slug + owner + size cap, valid 15 minutes. The upload route lives
at `POST /upload/ticket/:token` and is only mounted when MCP is enabled.

Authoring rules the connecting agent should follow (and which the resources
below spell out): self-contained `.html` with CSS/JS inlined, an `index.html`
entry point, relative links between pages, the `<link rel="stylesheet"
href="/app.css">` substrate, and no external CDNs. Image assets (`.svg`,
`.png`, `.jpg`, `.gif`, `.webp`) are written with `write_file` too.

## Resources

Pull-on-demand context, so the connecting agent doesn't arrive cold:

| URI | Purpose |
| --- | --- |
| `topbanana://guide/authoring` | The authoring contract (the build agent's own system prompt). |
| `topbanana://guide/design` | The `/app.css` design substrate: Tailwind utilities, daisyUI components, `data-theme` palettes. |
| `topbanana://guide/functions` | The server-side functions runtime contract (globals, handler shape, forbidden APIs). |
| `topbanana://templates` | The template catalog (id, label, description, whether functions are enabled, setup notes). |
| `topbanana://templates/{id}` | One template's authoring addendum, setup notes, and worked example pages. |

## Prompts

| Prompt | Arguments | Purpose |
| --- | --- | --- |
| `edit_page` | `slug`, `page` (default `index.html`), `goal` | Loads the current page + the conventions, framed as a specific edit. |
| `add_function` | `slug`, `purpose` | Loads the functions contract + a skeleton, framed as a new `/api` handler. |

## OAuth endpoints

| Path | Purpose |
| --- | --- |
| `GET /.well-known/oauth-protected-resource` | Resource metadata (RFC 9728). |
| `GET /.well-known/oauth-authorization-server` | Authorization-server metadata. |
| `POST /oauth/register` | Dynamic client registration (RFC 7591, minimal). |
| `GET /oauth/authorize` | Authorization-code endpoint (PKCE S256; reuses passkey session). |
| `POST /oauth/token` | Token endpoint; exchanges code + PKCE verifier for a bearer JWT. |

> Registered clients and pending authorization codes are held in memory
> (process-local). A multi-instance deployment behind a load balancer would
> need shared storage for these.
