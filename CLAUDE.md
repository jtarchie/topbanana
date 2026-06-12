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
| `task quality` | Fast quality pass: `task fmt` + `task tidy` + `task vuln` + `task sec` + `task licenses`. Run before pushing. |
| `task quality:deep` | Slow quality pass: `task quality` + `task bench` + `task fuzz` + `task nilaway`. Run when touching security-sensitive paths (auth, store, path validation, subdomain dispatch). |
| `task install:tools` | Installs every Go CLI the quality tasks depend on (`govulncheck`, `gosec`, `go-licenses`, `nilaway`, `benchstat`). Run once after cloning. |
| `task vuln` / `task sec` / `task licenses` / `task tidy` / `task nilaway` | Individual security and supply-chain gates. |
| `task test:cover` / `task test:cover:summary` | Coverage with HTML report / total-percent line. |
| `task bench` / `task bench:save` / `task bench:diff` | Run, snapshot, and benchstat-compare benchmarks. |
| `task fuzz` | Runs the path-traversal, subdomain, and HTML-lint fuzz targets for 30s each. |
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
When building or modifying agents for this project, keep in mind the `systemPrompt` (found in `internal/agent/agent_prompt.md`):

1.  **Strict File Types**: Create **only** `.html` files. CSS and JavaScript must be inlined.
2.  **Entry Point**: Every site must have an `index.html`.
3.  **No External Dependencies**: No external CDNs or frameworks. Everything must be self-contained.
4.  **Tooling**: Use the provided `write_file`, `read_file`, and `list_files` tools to manage the site content.
5.  **Relative Paths**: All links between pages must use relative URLs (e.g., `<a href="about.html">`).

## Code Organization
- `internal/store/`: the storage layer — `Store` owns compression-at-rest, slug-prefix path validation, metadata encoding, and the ARC cache over an `objectBackend` seam (S3 in production, in-memory for tests via `store.NewInMemory`). `keyspace.go` is the single registry of reserved bucket prefixes (`_snapshots/`, `_edits/`, `_acme/`, in-slug `_state/`) — never re-declare those literals.
- `internal/storetest/`: shared test helper. `storetest.New(t, n)` returns an in-memory store by default, or real S3/Minio when `AWS_ENDPOINT_URL` + `S3_BUCKET` are set — the whole suite runs deterministically in plain `go test`, and the same tests double as Minio conformance.
- `internal/server/`: the HTTP layer and composition root, split by concern (`routes.go`, `dispatch.go`, `proxy.go`, `sse.go`, `attachments.go`, `uploads.go`, `app_lifecycle.go`, per-feature topic files). **Import direction is enforced by a depguard rule**: nothing outside `cmd/` may import `internal/server`; push shared logic *down* into a domain package, never sideways.
- `internal/server/mcp_*.go`: the MCP edit surface + its OAuth authorization server. **Deliberately in-package** (not extracted): the tools share `Server`'s store/build/auth/registry plus a few private helpers (`invokeWithCAS`, `collectSubmissions`, `loadFunctionSource`, `storeUploadedAsset`). Keep new tools within that surface; if one needs more of `Server`, reconsider extraction first.
- `internal/agent/`: the build agent — `agent.go` (runner + tools), `instruction.go` (cache-stable prompt assembly), `state.go` (per-run state, anti-loop guard). Pure edit transforms live in `internal/textedit`, shared byte-for-byte with the MCP tools.
- `internal/build/`: build orchestration (`build.go`), the agent seam (`runner.go` — `Runner`/`RunRequest`), and the per-site sidecar (`meta.go` — `SiteMeta`/`ReadMeta`/`WriteMeta`).
- `internal/lint/`: the deterministic site-integrity gate (no LLM) every build/edit/relint runs. Check families: HTML parse + swallowed-attribute recovery; links and `#anchor` fragments (proxy-parity resolution via `resolveLinkTarget`/`resolveSiteTarget` in `links.go` — the single resolver shared by every path-shaped check); head hygiene (charset auto-fixed, lang, unique titles, meta description); form data-loss (unnamed controls, post-without-action, multipart/file inputs the API can't parse, inline-JS `fetch()` literals); dead interactions (label/for, duplicate ids, mailto:/tel: shape, undefined `on*` handlers, DOM lookups — the JS-aware checks stand down on dynamic-DOM or unparseable pages); self-containment (external scripts/styles with a Stripe allowlist, `http://` mixed content, unreferenced pages — template-skeleton pages exempt). Errors flow verbatim to the agent via `build.LintFixPrompt`, and `internal/build/friendly.go` maps stable message substrings to user-facing copy — **reword a lint message, update friendlyRules in the same change**. **Template skeletons and examples must stay lint-clean**: `skeleton_conformance_test.go` seeds every shipped skeleton and asserts zero errors, so a new check that flags one is either finding a real template bug or is too noisy to ship.
- `internal/model/`: LLM provider resolution logic.
- `internal/templates/sites/{id}/`: Site templates the user picks from. Each ships a `prompt.md` (JSON frontmatter + system addendum for the agent), an optional `skeleton/` (seed files), and a `README.md` (contributor docs).

## Adding a new site template
Every directory under `internal/templates/sites/` must contain:

1. **`prompt.md`** — JSON frontmatter with `label`, `description`, optional `checks`, optional `guide` (see below), optional `enables_functions`, optional `setup_notes` (end-user setup steps rendered on the manage page); markdown body after `---` is appended to the LLM system prompt.
2. **`README.md`** — contributor docs with these sections (omit Config / Gotchas if there's nothing to say):
   ```
   # {label}
   ## Purpose
   ## What ships
   ## Checks
   ## Completeness guide
   ## Config
   ## Gotchas
   ```
3. **`skeleton/`** (optional) — files seeded onto the filesystem before the agent runs.

### The `guide` frontmatter (owner-facing completeness checklist)
`guide` is the deterministic, **no-AI** "Is my site complete?" checklist rendered on the manage page (`internal/guide` + the card in `manage.html`). It is the advisory counterpart to `checks`: where `checks` are hard build invariants fed to the agent, `guide` items tell the *owner* what a credible site of this type still needs. Each item is `{id, label, why, how, detector, params?, page?, scope?, required?}`:

- `detector` is one of a fixed Go registry (`internal/guide/detectors.go`, asserted by `TestEveryTemplate_GuideIsWellFormed`): `tel_link`, `email_link`, `form`, `heading_matches` (`params.keywords`), `section_present` (`params.keywords` — heading match **plus** real body text, so an empty placeholder section fails), `address`, `map_link`, `min_images` (`params.min`), `min_links` (`params.min`).
- `scope` selects how the per-page results combine: `""`/`any-page` (default — present if any page matches), `every-page` (all pages must match), `specific-file` (only `page`, default `index.html`).
- `required: false` marks a nice-to-have — keep borderline detectors optional so a stray miss doesn't drag the badge to "incomplete".
- Detectors key on **semantic** elements/hosts (a `tel:` link, a `<form>`, a maps host), never CSS classes, so a design refactor never flips a result.

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
- **Subdomain Proxying**: The `subdomainMiddleware` in `internal/server/dispatch.go` is the heart of the routing. It strips the domain part to find the "slug" and uses that slug to query S3 (the serve path itself lives in `internal/server/proxy.go`).
- **S3 Path Structure**: Files are stored as `{slug}/{path}` within the bucket.
- **Logging**: Uses `slog`. Request logs include method, URI, status, latency, and host.
