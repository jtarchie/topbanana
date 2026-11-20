# Mail catching (mailcatch → Top Banana)

Fold `~/workspace/packager` (module `github.com/jtarchie/mailcatch`) into Top Banana as a per-site,
admin-only catch-all inbox. Receive-only; never sends.

## Decisions

| | Decision | Why |
|---|---|---|
| Readers | Owner + collaborators (`registry.canAccess`) | Same predicate as every site surface; no new access model. |
| Toggle | Super admin only | Accepting SMTP risks spam/abuse and the reputation of our dedicated IPv4 — an operator grant, like invites. |
| Retention | Per-site TTL, default 7d (owner picks 1h/1d/7d/30d) | Inbox people check later, not throwaway. |
| Integration | mailcatch becomes a Go library; topbanana imports it | One home for the ragel/fuzz/differential gate. |
| Storage | Driver interface in mailcatch; SQLite driver there (default), S3 driver in topbanana | Topbanana is S3-native and multi-instance-shaped. |
| Addresses | `*@{slug}.{DOMAIN}` + every attached custom domain | Wildcard MX we own makes subdomains instant; custom domains opt in via MX. |
| UI | Native `/mail/:slug` page in topbanana layout | mailcatch's web is anonymous/Mailinator-shaped; wrong assumptions + foreign look. |
| MCP | `list_mail` / `read_mail` / `delete_mail`, per-slug auth | Lets Claude verify a site's signup/contact flow end to end. No toggle tool. |
| Standalone | Retire `mailcatcher.divinesingularity.church` into topbanana | One deploy. Its public anonymous inboxes go away — intentional. |

## Phase 0 — mailcatch library refactor (packager repo)

Seams already exist (`smtpd.Sink`, `web.Reader`); promote and generalize them.

- Move `internal/{mailparse,smtpd}` → `mailparse/`, `smtpd/` (importable). New `mailstore/` (interface + `Message`), `mailstore/sqlite/` (current store). `internal/web`, `internal/cli` stay internal; standalone binary keeps working on SQLite.
- **Tenant**: `Message` gains `Tenant` (topbanana: slug) and `Domain` (recipient host). Every read/delete keyed `(tenant, id)` — keeps the "can't walk other inboxes" property.
- **ID → opaque string.** SQLite: decimal rowid. S3: key suffix.
- **ExpiresAt on the message**, set at delivery from the tenant's TTL. Reads filter `ExpiresAt > now` (keeps exact expiry with per-tenant TTL). SQLite schema bump (drop+recreate; data is ephemeral).
- **Dynamic domains**: replace `Config.Domains []string` with `Resolve func(ctx, domain) (tenant string, ttl time.Duration, ok bool)`. `smtpd.StaticDomains(...)` keeps CLI behavior. Called at RCPT so unknown/disabled domains still reject before DATA (no bounces).
- **Interface** (`mailstore.Store`): `Deliver`, `List(ctx, tenant, inbox, limit)` (`inbox==""` = all — admin wants a catch-all view), `Get`, `Delete`, `Reap`, `OverCapacity(tenant)`.
- **`mailstore/storetest`**: conformance suite (round trip, tenant isolation, expiry with injected clock, per-inbox cap, capacity). SQLite runs it; topbanana's S3 driver runs it too.
- Keep SQLite out of the import graph of `smtpd`/`mailparse` so topbanana doesn't pull `modernc.org/sqlite`.
- **Publish**: packager has no git remote. Push to `github.com/jtarchie/mailcatch` (tagged), or use a `go.work` locally until then. Topbanana's Docker build needs a fetchable module.
- Gate: `task` green; `FuzzSessionDifferential` untouched in behavior.

## Phase 1 — S3 driver (`internal/mail` in topbanana)

Domain package, not `internal/server` (depguard).

- Keyspace: new top-level `MailPrefix = "_mail/"` in `internal/store/keyspace.go`. Key `_mail/{slug}/{revRecvNanos}-{expiresUnix}-{rand}.eml`.
  - Top-level, not in-slug: site listings, snapshots, export, and `ListApps` never see mail.
  - Inverted receive time → `ListObjects` returns newest first; `List(limit)` = one call.
  - Expiry in the key → reads and reaper filter without HEAD requests.
  - Inbox/From/Subject/Size in object metadata (header-only ingest, same as mailcatch); filter-by-inbox is over the listed page.
- Use `WriteRaw`/`ReadRaw`/`ListPrefixInfo` — bypass the ARC cache (mail is write-once, read-rarely). Store raw bytes compressed as the store already does.
- `Deliver` = synchronous PutObject per message; 250 only after it returns (same guarantee as group commit).
- Reaper: background loop (pattern: `internal/events` sweep ticker) — delete expired keys, trim each slug past `MaxPerSite` (default 500), recompute per-slug bytes for `OverCapacity` (default 50 MiB/site).
- Runs `mailstore/storetest` against `storetest.New` (in-memory default, Minio when env set).

## Phase 2 — site model + routing

- `SiteMeta.Mail *MailSettings{Enabled bool, TTL Duration}` — mutated only via `UpdateMeta`.
- Registry: resolver `mailTenant(host) (slug, ttl, ok)`: `{slug}.{DOMAIN}` via subdomain parse, custom hosts via `domainIndex`, then require `Mail.Enabled`. Reject deeper subdomains. Rebuilt with the other indexes.
- Inboxes for subdomain + custom domains of one site share a namespace (keyed by slug); UI shows the `To` host.
- **Cascades**: site delete → delete `_mail/{slug}/`. Transfer → mail follows (keyed by slug, nothing to do). Remix/export/import → exclude mail.
- Reserve slug `mx` (the MX hostname is `mx.{DOMAIN}`; a site at that slug would collide).

## Phase 3 — SMTP listener

- `cmd/topbanana` flags: `--smtp-addr` (off by default), `--smtp-hostname` (default `mx.{DOMAIN}`), `--smtp-proxy-protocol`, `--smtp-impl` (default `gosmtp`; ragel soak can continue via flag).
- TLS: autocert cert for the MX host — add it to `HostPolicy`, pre-warm via `CertTracker`. Senders don't verify, so failure falls back to self-signed, never to no-STARTTLS.
- `fly.toml`: `[[services]]` port 25 → internal 2525, `handlers = ["proxy_proto"]`, no auto-stop (mail retries cover it, but a suspended machine defers everything). Dedicated IPv4 `168.220.83.70` already allocated.
- Rate/conn limits are per-process; fine at one machine. Revisit if scaled out.

## Phase 4 — UI

- Settings → **Mail** section:
  - Super admin: enable/disable.
  - Owner/collab: TTL picker, the catch-all addresses (`anything@{slug}.{DOMAIN}`, `anything@{custom}`), per-custom-domain MX status.
- `/mail/:slug` (canAccess): list (newest first, filter by inbox), message view, `/mail/:slug/:id/html` in sandboxed iframe with mailcatch's CSP (no scripts, no same-origin, `img-src data:`), `/raw` as `text/plain`, delete, delete-all.
- Attachments listed, never served (same as mailcatch). Revisit with Content-Disposition + nosniff if needed.
- Live updates: v1 plain reload; optional SSE via existing `sse.go` later.
- Canvas ⋯ menu link when enabled.

## Phase 5 — DNS diagnostics

- `domains.go`: alongside the CNAME target, compute expected MX. A CNAME'd custom host (`www.example.com → slug.DOMAIN`) **inherits our wildcard MX automatically**; an apex (flattened/A) needs an explicit `MX 10 mx.{DOMAIN}`.
- `get_domain_status` + manage page render MX record and live MX lookup. Warn when the host already has a different MX (enabling would take over their real mail).

## Phase 6 — MCP

`mcp_mail.go`: `list_mail(slug, inbox?, limit)`, `read_mail(slug, id)` (parsed text body + headers + attachment list), `delete_mail(slug, id)`. All via `mcpUserAndAuthorize`. Update server instructions.

## Phase 7 — deploy + retire standalone

1. DNS (Cloudflare, DNS-only): `mx.{DOMAIN}` A/AAAA → Fly IPs; `*.{DOMAIN}` MX 10 `mx.{DOMAIN}` (**check `*.{DOMAIN}` is A/AAAA, not CNAME — a CNAME can't coexist with MX**); `*.{DOMAIN}` TXT `v=spf1 -all`; `_dmarc.{DOMAIN}` `p=reject`.
2. Deploy, `nc mx.{DOMAIN} 25`, swaks to a test site, real mail from Fastmail.
3. Create/attach a topbanana site for `mailcatcher.divinesingularity.church`, enable mail, repoint its MX/A, verify, then `fly apps destroy mailcatcher-divinesingularity`.

## Risks / open

- Wildcard MX on the platform domain means any enabled slug becomes a spam target; the super-admin gate + per-site caps are the mitigation. Disabled sites reject at RCPT.
- S3 request cost: per-message PUT + reaper LISTs. At catcher volumes trivial; per-IP rate limit (60/min) bounds it.
- Port 25 on Fly has no official guarantee; standalone app proves it works on a dedicated IPv4.
