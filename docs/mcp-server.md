# MCP server

Top Banana exposes a [Model Context Protocol](https://modelcontextprotocol.io)
endpoint so an MCP client (e.g. Claude Code) can **author and manage sites on
behalf of a logged-in user**. Unlike the web `/build` flow, the MCP tools never
invoke the server-side LLM build agent — the connecting agent does all the
authoring by reading and writing files directly. Every tool is deterministic
(S3 reads/writes, metadata, slug allocation, and the pure-CPU lint pass).

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

| Tool | Purpose |
| --- | --- |
| `list_sites` | List the sites you own (title, template, created, URL). |
| `get_site` | Metadata + file list for one site. |
| `create_site` | Create a new empty site you own (no build agent). Enforces your app quota. |
| `read_file` | Read a file from a site. |
| `write_file` | Create/overwrite a file (`.html` stored with the right content type). |
| `list_files` | List file paths in a site. |
| `delete_file` | Delete a file. |
| `lint_site` | Run the deterministic lint checks and report problems to fix. |
| `list_runs` | List transcript keys for prior web-UI builds/edits. |
| `get_run_transcript` | Read one build/edit transcript (read-only). |

Authoring rules the connecting agent should follow (same as `static/agent_prompt.md`):
create only self-contained `.html` files with CSS/JS inlined, an `index.html`
entry point, relative links between pages, and no external CDNs or frameworks.

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
