# Readability & Structure Refactor — Backlog

Standing tracker for the codebase-organization effort. Findings come from a multi-agent
read-only audit (~130 items) against four goals: **single responsibility**, **result
objects**, **per-resource controllers**, and **RESTful routes** — plus testability,
dedup, and error-handling consistency.

Status legend: ⬜ todo · 🟦 in progress · ✅ done. Effort: S/M/L/XL.

## Approach

Foundational, low-risk work first (result/parameter objects, service extraction), then the
controller decomposition, then RESTful routes. Every step keeps `go build ./...` and
`go test ./...` green; commit one workstream/controller at a time.

## Status — what's landed

Done (each its own commit on `main`, `task fmt` green throughout):

- ✅ **Result objects** (high-value/shared): `textedit.ApplyEdit`→`EditResult`,
  `deleteApp`→`DeleteAppResult`, `ParseUploadTicket`→`UploadTicket`,
  `SumBytesUnderPrefix`→`PrefixStats`, `BuildArchive`→`archiveResult` (unexported).
- ✅ **siteRegistry** extracted from the `Server` god-struct (own file, own mutex, 10
  index methods); `Server` holds a single `*siteRegistry`.
- ✅ **server.go split** 2261→~1690 lines: `routes.go` (`mountRoutes`), `errors.go`
  (error handling), `dispatch.go` (subdomain/host routing + method-override).
- ✅ **Controller decomposition** — per-resource controllers embedding `*Server` (the
  shared base, à la pocketci's BaseController), each with a `register()` method:
  `assetsController`, `debugController`, `functionsController`, `accountController`,
  `adminController`, `sitesController`. `mountRoutes` is now a list of `register()` calls;
  route handlers live on controllers instead of `Server`.
- ✅ **method-override middleware** (e.Pre) — HTML forms drive PATCH/PUT/DELETE via a
  `_method` field or `X-HTTP-Method-Override` header. Parses/caches the urlencoded body
  while still POST (Go's `ParseForm` skips DELETE bodies).
- ✅ **REST verbs on destructive ops**: `DELETE /files/:slug` + `PATCH /files/:slug`
  (was POST .../delete, .../rename), `DELETE /apps/:slug` (was POST /settings/:slug/delete),
  `DELETE /account/sessions` (was POST /account/sign-out-everywhere), `PUT`/`DELETE
  /history/:slug` (was POST .../restore, .../delete). Forms + e2e/browser tests updated;
  verified against Minio (0 test-level failures).

Verified **non-issues** (audit false positives — code already correct, no change made):
`build.recordUsage` (the `Recorder.AddUsage` it calls already has an `if r == nil` guard);
`mcpOAuthState.clients` (both accesses are inside locked methods); `kv.go` "panics" (there
are none — the file has no `panic`).

Remaining (lower value or higher risk — deliberately deferred, all mechanical):
- Admin-panel verbs (super-admin only, audit-LOW): `enable`/`disable`/`quotas`→PATCH,
  user `delete`→DELETE, `sessions/revoke`→DELETE — JS-driven forms; method-override is in
  place, so each is a route+form+test change.
- Path-noun cleanups (POST stays, audit-LOW): `/relint/:slug`→`/sites/:slug/lint`,
  `/test/:slug/api/:name`→`.../functions/:name/test`, `/manage/:slug/remix`. (The broader
  `/workspace`→`/sites/:slug` noun consolidation is nav-wide churn, audit-LOW — kept stable.)
- Dedup `internal/archive` (tar+zstd): touches the legacy-PAX wire-format compat that
  CLAUDE.md flags as load-bearing — do with care + round-trip tests, not in a rush.
- Result-objects long tail + parameter objects (Workstreams 2/3 below): high-churn,
  low-individual-value; `Subscribe`/`runnerForTier` skipped (12+ test sites each, hotpath).

## Preserve (do not churn — already well-factored)

- `internal/textedit` — pure stateless edit transforms shared by the build agent **and** MCP. Single home for edit/validation logic.
- `internal/state` — clean `Store` interface + `Memory`/`S3Store` + conformance tests. The model `store` should follow.
- Per-feature handler file split (`assets.go`, `admin_users.go`, `workspace.go`, …) — the seams controllers lift from.
- `assets.go` REST shape (`GET`/`PATCH`/`DELETE` via `fetch`) — the template to copy.
- Agent tool result-objects-as-data (LLM in-loop recovery), `toolGuard` anti-loop, `fetch_reference` SSRF guard, `state` ETag CAS, MCP optionality behind `--mcp-secret` + isolated OAuth state machine.
- Supporting-package cohesion: `snapshot`, `editrec`, `events`, `model`, `templates`, `portable`, `assets`.

---

## Workstream 1 — Controller decomposition (SRP + controllers) · XL

The `Server` struct (`internal/server/server.go:93`) is a god object: 15+ deps + 4 index
maps + ~53–80 handler methods, all routes registered in a 2261-line `New()`.

Target pattern (cf. pocketci `BaseController`): a `BaseController` holds shared deps +
helpers; per-resource controllers embed it and own `Register(g *echo.Group)`. `New()`
becomes a thin orchestrator.

- ⬜ XL Extract domain services out of `Server` **first** (enables the split):
  - `SiteRegistry` → new `internal/server/site_registry.go`: `domainMu` + the four maps + `ownerOf`/`isPrivate`/`setOwner`/`markSlug`/`countAppsFor`/`slugExists`/`lookupCustomDomain`/`rebuildDomainIndex` (`server.go:447-617`).
  - `Renderer`/views → new `internal/server/views.go`: `render`/`injectChrome`/`injectEditToolbar` (`server.go:755`).
  - Split `server.go`: `routes.go` (registration), `errors.go` (`httpErrorHandler`/`httpErr`/`notFound`), `proxy.go` (`subdomainMiddleware`/`dispatchSite`/`proxyHandler`/`pathRouteHandler`).
- ⬜ L `BaseController{store, build, snapshot, events, auth, registry, renderer, domain}` + helpers: `render`, `httpErr`, `respondHTMLOrJSON` (generalize `wantsHTML` `server.go:902`), `emailFromContext`, `slugParam`. Reuse `requireUser`/`requireSlugOwnership` as-is.
- ⬜ XL Per-resource controllers (lift existing handlers; migrate smallest-first, one commit each):
  - `AssetsController` (already REST — do first), `SitesController`, `FunctionsController`, `AccountController` (rename `auth_handlers.go`→`account.go`), `AdminController`, `DebugController`, `ProxyController` (public delivery + `/api/:name`), `MCPController` (wrap the already-isolated `mcp_*.go`).
- ⬜ M Move route registration into per-controller `Register` methods; `New()` orchestrates.

Related supporting findings: domain index conflates routing/ownership/privacy (`server.go:113-126`); proxy dispatch mixed with admin routes (`server.go:350-424`, `1551-1654`); template render logic scattered (`server.go:755-833`, `1654-1757`).

## Workstream 2 — Result objects (result objects) · M

Functions returning 3–4 unnamed values; introduce a named struct, keep behavior. **High-value first:**

| Function | Loc | Result struct |
|---|---|---|
| `textedit.ApplyEdit` (shared agent+MCP) | `textedit/textedit.go:34` | `EditResult{Content,Count,Note}` |
| `deleteApp`/`deleteAppsOwnedBy`/`reassignAppsOwnedBy` | `server/server.go:1934,1971,2008` | `DeleteAppResult`/`BulkAppResult` |
| `agent.fetchAndInline` | `agent/fetch_reference.go:203` | `FetchResult{HTML,FinalURL,Truncated}` |
| `auth.ParseUploadTicket` | `auth/upload_ticket.go:71` | `UploadTicket{Email,Slug,MaxBytes}` |
| `snapshot.BuildArchive` (also unexport) | `snapshot/snapshot.go:291` | `ArchiveResult` |
| `build.runnerForTier`/`LLMForTier`/`resolveTailwindCLI` | `build/build.go:342,404`, `css_compile.go:179` | `RunnerResolution`/`LLMResolution` (+ `error` not `ok`) |
| `events.Subscribe` | `events/events.go:174` | `SubscribeResult{History,Channel,Terminal}` |
| `store.SumBytesUnderPrefix` | `store/store.go:472` | `PrefixStats{TotalBytes,ObjectCount}` |

Control-signal anti-pattern `(bool,error)` = "handled": `disposeOwnedSites`/`refuseLastSuperAdmin` (`admin_users.go:354,374`) → return a sentinel error.

**Long tail (apply opportunistically):** lint `checkLink` (pointer→`[]Error`) `lint.go:371`, `AutoFixDesignSubstrate` `lint.go:197`; sandbox `coerce*` inconsistent `(T,bool)`/`(T,error)` + `validateField` `*validationError`→`error` `validate.go:62,196-256`; `collectSubmissions` `data.go:51`; `actionsFor` `files.go:104`; `summarizeBuilds` `system.go:135`; `splitPage` `visual_edit.go:178`; `parseFrontmatter` `templates.go:223`; `appendFileMatches` `agent.go:1244`; `readPages` `css_compile.go:86`; `fetchServed` `debug.go:286`; `mcpJSON` unused middle return `mcp_server.go:110`; `findUnconsumedFor` `(*Invite,bool,error)` `invites.go:174`; `parseKey` `editrec.go:464`.

## Workstream 3 — Parameter objects (SRP) · M

Functions with 7+ positional params:

- ⬜ M `build.buildAndLint`/`PolishPass` → `PipelineContext{Slug,Template,Attachments,IsEdit,Recorder,LogKey}` (`build.go:708,669`).
- ⬜ M `agent.buildAgentTools` / every `newXxxTool` → `ToolBuildContext{Store,Slug,Emit,State,Tracker,Template}` (`agent.go:510`).
- ⬜ M `sandbox.Invoke` (7 params) → `InvokeRequest{Source,Name,Request,Snapshot,LogFn}` (`sandbox.go:114`).
- ⬜ S lint `checkHTMLLinks`/`checkNodeLinks`/`checkLink` → `LinkCheckContext{FileSet,EnablesFns}` (`lint.go:339-371`).
- ⬜ S `build` runner cache key → `RunnerCacheKey{ModelID,ReasoningEffort}` (`build.go:356`).

## Workstream 4 — RESTful routes (full verbs) · L

Add a method-override middleware so the 27 POST `<form>`s can send `_method=DELETE|PATCH|PUT`
(several flows already use `fetch`, e.g. `workspace.html:325`). Unify nouns under `/sites/:slug`;
301/302 redirect old paths. Update template action paths on move.

| Current | Proposed |
|---|---|
| `GET /workspace/:slug`, `/manage/:slug`, `/edit/:slug` | `GET /sites/:slug` (+ `/sites/:slug/{visual,settings,history,data,files,debug}`) |
| `POST /relint/:slug` | `POST /sites/:slug/lint` |
| `POST /test/:slug/api/:name` | `POST /sites/:slug/functions/:name/test` |
| `POST /files/:slug/delete`, `/rename` | `DELETE /sites/:slug/files/:path`, `PATCH /sites/:slug/files/:path` |
| `POST /settings/:slug/delete` | `DELETE /sites/:slug` |
| `POST /history/:slug/restore`, `/delete` | `PUT /sites/:slug/history/:snapshot`, `DELETE /sites/:slug/history/:snapshot` |
| `POST /manage/:slug/remix`, `/apps/:slug/transfer` | `POST /sites/:slug/remix`, `POST /sites/:slug/transfer` |
| `POST /account/sign-out-everywhere` | `DELETE /account/sessions` |
| `POST /admin/users/:email/{disable,enable,quotas}` | `PATCH /admin/users/:email` |
| `POST /admin/users/:email/delete` | `DELETE /admin/users/:email` |
| `POST /admin/users/:email/sessions/revoke` | `DELETE /admin/users/:email/sessions` |

Keep as-is (REST N/A): SSE `/events/:slug`, `/status/:slug`, subdomain proxy, OAuth well-knowns, `/mcp`, `/auth/*` (passkey lib).

## Workstream 5 — Deduplication / reuse · M

- ⬜ M tar+zstd codec → new `internal/archive` (snapshot + portable share it; keep snapshot's PAX constants as source of truth). `snapshot.go:291-385`, `portable.go:101-302`.
- ⬜ M function-testing web vs MCP → shared `testFunctionInternal` (`api.go:451` ↔ `mcp_functions.go:226`).
- ⬜ M user-deletion cascade → `UserDeletionService` (`admin_users.go:292` ↔ `auth_handlers.go:225`).
- ⬜ M lint HTML tree-walk (4–5 copies) → `WalkHTML(n, visit)` (`lint.go:168-351`, `inline_scripts.go:19`).
- ⬜ L agent write-tools (4 copies, ~300 LOC) → one shared write handler + `ToolBuildContext` (`agent.go:825-1160`).
- ⬜ S `store` metadata encode/decode (3 sites) → `encodeMetadata`/`decodeMetadata`; `parseS3Object` for Read/ReadRaw (`store.go:60-180,402-434`).
- ⬜ S `describe.go`/`caption.go` LLM-JSON call → `callLLMForJSON` (`describe.go:107`, `caption.go:63`).
- ⬜ S server helpers: `emailFromContext`, `emailFromForm`, move `urlEscape` to a shared spot, fold `redirectToWorkspace`/`redirectToManage` into one (`history.go:109`, `server.go:734-753`).
- ⬜ S MCP: `canMutateFunctions` (enables-functions check, 3 sites) `mcp_functions.go:39,200,280`; `mcpErr(op,path,err)` for uniform tool wrapping.

## Workstream 6 — Error handling & logging consistency · M

- ⬜ M `AppError{Code,UserMessage,LogMessage,Err}` mapped centrally in `httpErrorHandler` (`server.go:835`); define sentinels (`ErrNotFound`/`ErrConflict`/…).
- ⬜ S Fix `httpErr` to wrap with `%w` (currently `%s`, `server.go:800`); replace `"msg: "+err.Error()` concatenations with `fmt.Errorf(... %w ...)`.
- ⬜ S `slog.Warn` on silently-skipped corrupted records in `auth` list ops (`userstore.go:158`, `invites.go:153`).
- ⬜ S Standardize store error prefixes `store: op key: %w` (`store.go`, `state/s3store.go`); document/justify the `isPreconditionFailed` string-match fallback (`s3store.go:117`).
- ⬜ S Standardize auth error prefixes on `auth: op` (`userstore.go`/`invites.go`/`types.go`/`migrate.go`).

## Workstream 7 — Testability seams · M

- ⬜ M `store` interfaces `FileStore`/`RawStore`/`AdminStore` (`store.go:19`) + a `Memory` double mirroring `state` → fast unit tests for `agent`/`snapshot`/`portable` without Minio.
- ⬜ M Extract agent write-tool logic into standalone testable handlers (ties to WS5 write-handler).
- ⬜ M MCP tool handlers: named funcs on Server instead of capture-heavy closures (`mcp_server.go`/`mcp_functions.go`) for testability.

## Workstream 8 — Latent correctness issues (verify at line, then fix) · S

Surfaced by the audit; fold into the relevant phase.

- ⬜ S `build.recordUsage` nil-receiver crash via best-effort paths → nil-check (`build.go:681`).
- ⬜ S sandbox `kv.put`/`kv.incr` **panic** on validation failure → return JS-catchable errors (`kv.go:43-98`).
- ⬜ S `mcpOAuthState` clients map read without the lock → lock or document immutability + race test (`mcp_oauth.go:242`).
- ⬜ S `lastEditedFor` O(N²) on `/apps` → batch or cache (`server.go:1095`).

## Naming (low priority)

Standardize on *site* (user-facing noun) vs *slug* (internal id); `auth_handlers.go`→`account.go`;
`sanitizeStem`→`filenameToSlug`; doc the `mcpBaseURL`/`mcpSiteURL`/`mcpPageURL` trio.
