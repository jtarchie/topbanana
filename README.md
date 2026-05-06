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
