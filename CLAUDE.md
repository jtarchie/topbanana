# Agent Guide for Top Banana

## Overview
Top Banana is a "vibe coding" hosting platform that uses LLM agents to build and host static HTML applications. Each application is hosted under a unique subdomain, with files stored in an S3-compatible backend (e.g., Minio).

> **Naming history.** The project was BuildABear → Bloomhollow → **Top Banana**. The Go module is `github.com/jtarchie/topbanana` and the binary is `topbanana`. The workspace directory stays `/workspace/buildabear/`. The current per-site sidecar is `.topbanana.json` (PAX prefix `TOPBANANA.*`, export manifest `topbanana-export.json`); `.bloomhollow.json` / `BLOOMHOLLOW.*` / `bloomhollow-export.json` and `.buildabear.json` / `BUILDABEAR.*` remain **intentional legacy compat reads** so pre-rebrand sites and archives still resolve. Do not remove them.

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
| `task css` | Recompiles the embedded admin-UI stylesheet (`internal/assets/app.css`) from `app.input.css`. Run after editing the admin templates or input CSS. |
| `task vendor:daisyui` | Re-vendors the daisyUI npm package into `internal/assets/daisyui` (then bump `DaisyUIVersion` in `internal/assets/embed.go` + run `task css`). |
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

## CSS pipeline (self-hosted Tailwind + daisyUI — no CDN)
The design substrate is **compiled, not CDN-loaded**. daisyUI v5 is vendored in `internal/assets/daisyui/` and the Tailwind v4 **standalone CLI** (Node-free) compiles it:

- **The canonical substrate is `/app.css`** — a single `<link rel="stylesheet" href="/app.css">`. There are **no** CDN tags anywhere: the agent prompt, lint, and every skeleton/example HTML emit `/app.css`. (`internal/build/css_compile.go` still keeps regexes to *strip* legacy `cdn.jsdelivr.net` tags from old stored pages on re-edit.)
- **Admin UI**: a single sheet `internal/assets/app.css` is precompiled (`task css`) from `internal/assets/app.input.css`, embedded via `internal/assets/embed.go`, and served at `/app.css` by `appCSSHandler`. Committed so `go run` works without the CLI. `layout.html` / `visual_edit.html` link `/app.css`.
- **User sites**: `build.Service.OptimizeCSS` (`internal/build/css_compile.go`) runs the CLI over the site's actual HTML (`@plugin "daisyui" { themes: all }`), writes `{slug}/app.css` (served at `/app.css` on the site host), and injects the `/app.css` link into pages that lack it. It runs after every web build/edit (`Service.Start`) **and** from the MCP `lint_site` tool, so directly-authored (Claude Code / MCP) sites get the same self-hosted sheet. **No fallback**: if the compile is skipped (no CLI) or fails, `/app.css` 404s and the page is unstyled — so the CLI must be present wherever sites are built or linted (it's in the Docker image; dev needs `tailwindcss`/`npx`).
- **Lint**: `checkDesignSubstrate` requires the `/app.css` link (auto-fixed by `AutoFixDesignSubstrate`); `checkLink` exempts `/app.css` from the broken-link check since it's created post-lint by `optimizeCSS`.
- **CLI resolution**: `--tailwind-cli` / `TAILWIND_CLI`, else `tailwindcss` on PATH, else `npx @tailwindcss/cli`, else skip. The Docker image carries the `tailwindcss-linux-*-musl` standalone binary at `/usr/local/bin/tailwindcss`.

**Gotchas:**
- The `*-musl` standalone binary is **not fully static** — the runtime image must install `libstdc++ libgcc` or it fails with relocation errors.
- daisyUI v5 components are a fixed layer (**not** content-purged); only the Tailwind utility layer is purged, and `@source not <daisyui dir>` is required or the output balloons (~52 KB vs ~260 KB+).
- `themes: all` is intentional so Theme Studio can switch themes without a recompile.
- `optimizeCSS` runs *after* lint, so a page links `/app.css` before the file exists — both the lint broken-link check and the proxy must tolerate that ordering.

## Implementation Details (for developers)
- **Subdomain Proxying**: The `subdomainMiddleware` in `server.go` is the heart of the routing. It strips the domain part to find the "slug" and uses that slug to query S3.
- **S3 Path Structure**: Files are stored as `{slug}/{path}` within the bucket.
- **Logging**: Uses `slog`. Request logs include method, URI, status, latency, and host.
