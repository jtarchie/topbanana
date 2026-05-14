# BuildABear

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

The server starts on port `8080`. Visit `http://localhost:8080` to see the landing page.

## Configuration

All options are available as CLI flags or environment variables.

| Flag | Env Var | Default | Description |
|---|---|---|---|
| `--port` | | `8080` | HTTP listen port |
| `--domain` | | `localhost` | Base domain for subdomain routing |
| `--admin-username` | `ADMIN_USERNAME` | `admin` | Username for the admin HTTP Basic Auth gate |
| `--admin-password` | `ADMIN_PASSWORD` | *(required)* | Password for the admin gate; server refuses to start without it |
| `--s3-bucket` | `S3_BUCKET` | *(required)* | S3 bucket name |
| `--s3-endpoint-url` | `AWS_ENDPOINT_URL` | | Override S3 endpoint (e.g. Minio) |
| `--llm-model` | `LLM_MODEL` | | LLM model string |
| `--llm-base-url` | `LLM_BASE_URL` | | LLM provider base URL |
| `--llm-api-key` | | | LLM provider API key |
| `--cache-size` | | `1024` | ARC cache entry limit |

## Development

```bash
task fmt          # Format, lint, and test
task local        # Start app with Minio + LM Studio
task minio:start  # Start Minio in background
task minio:stop   # Stop Minio
task minio:ready  # Start Minio if not running
```

## Custom Domains with Cloudflare

A BuildABear site can be served on any external domain (e.g. `myblog.com`) by attaching the hostname under **Settings → Custom domains** and pointing DNS at your origin. Putting Cloudflare in front gives you free TLS and a global cache; BuildABear already emits the right cache headers, so the Cloudflare config is small.

### 1. Add the domain in BuildABear

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

If you're running BuildABear behind a bare IP, use `A` records instead of `CNAME` — same idea.

### 3. SSL/TLS

BuildABear listens on plain HTTP. Terminate TLS at Cloudflare (or with a Caddy/nginx reverse proxy on the origin):

- **SSL/TLS → Overview → Encryption mode**:
  - `Full` (or `Full (strict)`) if you put a TLS-terminating proxy in front of BuildABear.
  - `Flexible` if BuildABear is exposed over plain HTTP — Cloudflare ↔ visitor is HTTPS, Cloudflare ↔ origin is HTTP. Easier to set up; weaker than Full.
- **SSL/TLS → Edge Certificates → Always Use HTTPS**: on.

### 4. Caching

BuildABear sends explicit cache directives:

| Path on a custom domain | `Cache-Control` |
| --- | --- |
| HTML / CSS / JS / images | `public, max-age=300, s-maxage=3600` (+ `Vary: Accept-Encoding`, `ETag`) |
| `/api/*` (dynamic state) | `no-store, private` (+ `Pragma: no-cache`, `Vary: *`) |

Cloudflare's default cache only stores certain file extensions, so extensionless URLs like `/` won't be cached unless you say so. Create **one** Cache Rule:

- **Caching → Cache Rules → Create rule**
- **Name**: `BuildABear — respect origin headers`
- **When incoming requests match**: `Hostname` equals `myblog.com` (add a second `or` for `www.myblog.com`)
- **Then**:
  - **Cache eligibility**: *Eligible for cache*
  - **Edge TTL**: *Use cache-control header from origin*
  - **Browser TTL**: *Use cache-control header from origin*

That single rule is enough: Cloudflare obeys the `no-store` on `/api/*` and the public TTL on static content. No bypass-cache rule needed for `/api/` because the origin's `no-store` already opts those responses out of the edge cache.

### 5. Cache invalidation

Site edits propagate within the 5-minute `max-age` window. If you need a change live immediately, purge from **Caching → Configuration → Purge cache** (single-file purge by URL is enough — you don't need a full purge).

## Agent Constraints

The LLM agent operates under strict rules (defined in [static/agent_prompt.md](static/agent_prompt.md)):

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
