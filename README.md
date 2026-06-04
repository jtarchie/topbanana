# Top Banana

A "vibe coding" hosting platform where LLM agents build and host static HTML applications. Describe what you want, and an AI agent generates a self-contained site hosted under a unique subdomain.

## How It Works

1. Submit a prompt via the `/build` endpoint
2. An LLM agent generates `.html` files (with inlined CSS and JS) using S3-backed file tools
3. The site is hosted immediately at `{slug}.{domain}` via subdomain routing
4. Build progress streams live via Server-Sent Events

## Architecture

- **Subdomain routing** — requests to `*.localhost` (or your configured domain) are proxied to an S3 prefix matching the subdomain slug
- **Agentic builder** — LLM agent with `write_file`, `read_file`, `list_files`, and `list_assets` tools; orchestrated via Google ADK
- **S3 storage** — AWS S3 SDK v2 with path-style addressing; supports Minio for local dev
- **ARC cache** — adaptive replacement cache in front of S3 reads
- **HTML linting** — validates generated HTML and relative links after each build

## Prerequisites

- [Go](https://go.dev/) 1.21+
- [Task](https://taskfile.dev/)
- [Minio](https://min.io/) (for local S3-compatible storage)
- An LLM provider — defaults to [LM Studio](https://lmstudio.ai/) at `http://localhost:1234/v1`

## Quick Start

```bash
# Start Minio and run the server against LM Studio
task local
```

The server starts on port `8080`. Visit `http://lvh.me:8080` to see the landing page.

### First-run: enrolling the super admin

Authentication is passkey-based — there is no admin password. On every startup, the server seeds a user record for `--super-admin-email` (defaults to `admin@local` in `task local`) and issues a one-shot bootstrap invite the first time that account has no registered credentials. The invite URL is logged as:

```
auth.bootstrap.invite_pending email=admin@local url=https://lvh.me/register?invite=<token> expires=...
```

For local HTTP dev, **rewrite the scheme and add the port** — turn that into `http://lvh.me:8080/register?invite=<token>` and open it in a browser. The `/register` page binds a passkey to the super-admin account; subsequent visits to `/login` use that passkey. The invite is reused on every restart until you finish enrolling, then it stops appearing in the logs.

If you ever lose the passkey, delete the user from Minio (`rm -rf /tmp/topbanana-minio/topbanana/_auth/users/`) and restart — bootstrap will fire again.

## Configuration

All options are available as CLI flags or environment variables.

| Flag | Env Var | Default | Description |
|---|---|---|---|
| `--port` | `PORT` | `8080` | HTTP listen port |
| `--domain` | `DOMAIN` | `localhost` | Base domain for subdomain routing |
| `--super-admin-email` | `SUPER_ADMIN_EMAIL` | *(required)* | Email of the seeded super admin; bootstrap invite is logged at startup until enrolled |
| `--insecure-cookies` | `INSECURE_COOKIES` | off | Allow non-Secure cookies; required for the cookie/session flow over plain HTTP locally |
| `--s3-bucket` | `S3_BUCKET` | *(required)* | S3 bucket name |
| `--s3-endpoint-url` | `AWS_ENDPOINT_URL` | | Override S3 endpoint (e.g. Minio) |
| `--llm-model` | `LLM_MODEL` | | LLM model string |
| `--llm-base-url` | `LLM_BASE_URL` | | LLM provider base URL |
| `--llm-api-key` | `LLM_API_KEY` | | LLM provider API key |
| `--cache-size` | | `1024` | ARC cache entry limit |

## Development

```bash
task fmt          # Format, lint, and test
task local        # Start app with Minio + LM Studio
task minio:start  # Start Minio in background
task minio:stop   # Stop Minio
task minio:ready  # Start Minio if not running
task test:llm     # Opt-in real-model integration tests (see below)
```

### Real-model integration tests

Every other test stubs the agent. `task test:llm` instead drives the **real**
agent loop (prompt → tools → lint/retry → CSS → describe) against a local model,
catching prompt/tool/lint-loop regressions the stubs can't. It requires LM Studio
running plus Minio, is **opt-in** (gated by `TOPBANANA_LLM_E2E=1`), is slow
(minutes), and is **excluded from the default `go test`/`task fmt` run** — without
the gate the tests skip. Assertions are structural invariants (build completes,
`index.html` non-empty, lint clean, a `write_file` call happened), not exact model
output.

`task lmstudio:ready` loads `google/gemma-4-12b` with a **16K context** — this
matters: the build agent's system prompt is ~4–7K tokens, so LM Studio's default
4096 context starves the model (it never gets room to call `write_file`). If you
load a model by hand, give it at least a 16K context.

## Custom Domains with Cloudflare

A Top Banana site can be served on any external domain (e.g. `myblog.com`) by attaching the hostname under **Settings → Custom domains** and pointing DNS at your origin. Putting Cloudflare in front gives you free TLS and a global cache; Top Banana already emits the right cache headers, so the Cloudflare config is small.

### 1. Add the domain in Top Banana

Open **Settings** for the site (e.g. `http://your-domain/settings/{slug}`) and add the hostnames you'll be using — one per line:

```
myblog.com
www.myblog.com
```

Save. The server rebuilds its host → slug index immediately; requests carrying those `Host` headers now resolve to that site.

### 2. Point DNS at your origin (Cloudflare)

In the Cloudflare dashboard for the zone:

- **Apex (`myblog.com`)** — CNAME record to your origin hostname (e.g. `origin.example.com`). Cloudflare flattens CNAMEs at the apex automatically.
- **`www.myblog.com`** — CNAME to the same origin hostname.
- Set **Proxy status: Proxied** (orange cloud) on both records so traffic flows through Cloudflare's edge.

If you're running Top Banana behind a bare IP, use `A` records instead of `CNAME` — same idea.

### 3. SSL/TLS

Top Banana listens on plain HTTP. Terminate TLS at Cloudflare (or with a Caddy/nginx reverse proxy on the origin):

- **SSL/TLS → Overview → Encryption mode**:
  - `Full` (or `Full (strict)`) if you put a TLS-terminating proxy in front of Top Banana.
  - `Flexible` if Top Banana is exposed over plain HTTP — Cloudflare ↔ visitor is HTTPS, Cloudflare ↔ origin is HTTP. Easier to set up; weaker than Full.
- **SSL/TLS → Edge Certificates → Always Use HTTPS**: on.

### 4. Caching

Top Banana sends explicit cache directives:

| Path on a custom domain | `Cache-Control` |
| --- | --- |
| HTML / CSS / JS / images | `public, max-age=300, s-maxage=3600` (+ `Vary: Accept-Encoding`, `ETag`) |
| `/api/*` (dynamic state) | `no-store, private` (+ `Pragma: no-cache`, `Vary: *`) |

Cloudflare's default cache only stores certain file extensions, so extensionless URLs like `/` won't be cached unless you say so. Create **one** Cache Rule:

- **Caching → Cache Rules → Create rule**
- **Name**: `Top Banana — respect origin headers`
- **When incoming requests match**: `Hostname` equals `myblog.com` (add a second `or` for `www.myblog.com`)
- **Then**:
  - **Cache eligibility**: *Eligible for cache*
  - **Edge TTL**: *Use cache-control header from origin*
  - **Browser TTL**: *Use cache-control header from origin*

That single rule is enough: Cloudflare obeys the `no-store` on `/api/*` and the public TTL on static content. No bypass-cache rule needed for `/api/` because the origin's `no-store` already opts those responses out of the edge cache.

### 5. Cache invalidation

Site edits propagate within the 5-minute `max-age` window. If you need a change live immediately, purge from **Caching → Configuration → Purge cache** (single-file purge by URL is enough — you don't need a full purge).

## Agent Constraints

The LLM agent operates under strict rules (defined in [internal/agent/agent_prompt.md](internal/agent/agent_prompt.md)):

- Only `.html` files — CSS and JS must be inlined
- Every site requires an `index.html` entry point
- No external CDNs or frameworks; everything self-contained
- All links between pages must use relative URLs

## Code Layout

```
main.go           CLI entry point and configuration
server.go         HTTP server, subdomain routing, SSE build events
agent.go          LLM agent orchestration and file tools
s3store.go        S3 storage abstraction with ARC cache
caption.go        AI-generated image captions
lint.go           HTML validation and link checking
sluggen.go        Random subdomain slug generation
internal/model/   LLM provider resolution (Claude, OpenAI, LM Studio)
static/           Embedded assets and agent system prompt
templates/        HTML templates and starter site templates
```

## Starter Templates

New sites can be bootstrapped from templates: blank, birthday, email-capture, event, landing-page, link-in-bio, portfolio, pricing, case-study, restaurant, resume, and waitlist.
