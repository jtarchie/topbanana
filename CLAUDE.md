# Agent Guide for Bloomhollow

## Overview
Bloomhollow is a "vibe coding" hosting platform that uses LLM agents to build and host static HTML applications. Each application is hosted under a unique subdomain, with files stored in an S3-compatible backend (e.g., Minio).

## Architecture
- **Multi-tenant Static Hosting**: The server uses subdomain routing. Requests to `*.localhost` are intercepted by the `subdomainMiddleware` and proxied to an S3 bucket where each site is a folder/prefix identified by its subdomain slug.
- **Agentic Builder**: When a user submits a prompt via `/build`, the system spawns an LLM agent. This agent has access to tools to create, read, and list files within that specific site's S3 prefix.
- **Storage**: Uses AWS S3 SDK (v2) with path-style addressing to support Minio.

## Development Workflow

### Prerequisites
- [Task](https://taskfile.dev/) installed.
- A local LLM provider (e.g., LM Studio) running at `http://localhost:1234/v1` (default).

### Essential Commands
Use `task` for development automation.

| Command | Description |
| --- | --- |
| `task fmt` | Runs linters on the codebase |
| `task local` | Starts the application locally, ensuring Minio is ready and pointing to LM Studio. |
| `task minio:start` | Starts a background Minio server. |
| `task minio:stop` | Stops the running Minio server. |
| `task minio:ready` | Verifies or starts Minio if it's not currently running. |

NOTE: Please run `task fmt` and resolve all issues after finish writing code.

### Configuration
The application is configured via CLI flags and environment variables (via `kong`).

- `S3_BUCKET`: Name of the bucket to use for all sites.
- `AWS_ENDPOINT_URL`: Override S3 endpoint (e.g., for Minio).
- `LLM_MODEL`: The model string (e.g., `lmstudio/google/gemma-4-26b-a4b`).
- `LLM_BASE_URL`: URL of the LLM provider.

## Agent Instructions & Constraints
When building or modifying agents for this project, keep in mind the `systemPrompt` (found in `static/agent_prompt.md`):

1.  **Strict File Types**: Create **only** `.html` files. CSS and JavaScript must be inlined.
2.  **Entry Point**: Every site must have an `index.html`.
3.  **No External Dependencies**: No external CDNs or frameworks. Everything must be self-contained.
4.  **Tooling**: Use the provided `write_file`, `read_file`, and `list_files` tools to manage the site content.
5.  **Relative Paths**: All links between pages must use relative URLs (e.g., `<a href="about.html">`).

## Code Organization
- `internal/model/`: LLM provider resolution logic.
- `internal/templates/sites/{id}/`: Site templates the user picks from. Each ships a `prompt.md` (JSON frontmatter + system addendum for the agent), an optional `skeleton/` (seed files), and a `README.md` (contributor docs).
- `static/`: Static assets like the landing page and agent system prompts.
- `s3store.go`: The core storage abstraction layer.
- `server.go`: The web server implementation and subdomain proxying logic.

## Adding a new site template
Every directory under `internal/templates/sites/` must contain:

1. **`prompt.md`** — JSON frontmatter with `label`, `description`, optional `checks`, optional `enables_functions`, optional `setup_notes` (end-user setup steps rendered on the manage page); markdown body after `---` is appended to the LLM system prompt.
2. **`README.md`** — contributor docs with these sections (omit Config / Gotchas if there's nothing to say):
   ```
   # {label}
   ## Purpose
   ## What ships
   ## Checks
   ## Config
   ## Gotchas
   ```
3. **`skeleton/`** (optional) — files seeded onto the filesystem before the agent runs.

## Implementation Details (for developers)
- **Subdomain Proxying**: The `subdomainMiddleware` in `server.go` is the heart of the routing. It strips the domain part to find the "slug" and uses that slug to query S3.
- **S3 Path Structure**: Files are stored as `{slug}/{path}` within the bucket.
- **Logging**: Uses `slog`. Request logs include method, URI, status, latency, and host.
