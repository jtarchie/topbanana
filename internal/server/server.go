// Package server is the HTTP layer — Echo routes, handlers, template
// rendering, the subdomain proxy, request validation, and upload handling.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"
	slogecho "github.com/samber/slog-echo"
	"github.com/tdewolff/minify/v2"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/portable"
	"github.com/jtarchie/topbanana/internal/sandbox"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/state"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// SystemInfo captures read-only platform configuration the system dashboard
// surfaces. Populated from CLI flags in cmd/topbanana/main.go — these values
// don't live anywhere else on the wired-up server (build.Service and
// store.Store keep them in private fields), so we hand them in directly
// rather than threading getters through every package.
type SystemInfo struct {
	LLMTiers           model.TierMap
	LLMBaseURL         string
	LLMReasoningEffort string
	S3Endpoint         string
	S3Bucket           string
	SnapshotKeep       int
	EditsKeep          int
}

// Deps holds the dependencies the server needs. Wired up in cmd/topbanana.
type Deps struct {
	Store    *store.Store
	Build    *build.Service
	Events   *events.Tracker
	LLM      adkmodel.LLM
	Sandbox  *sandbox.Manager
	State    state.Store
	Snapshot *snapshot.Service
	// Auth drives the passkey/multi-tenant identity surface. main.go
	// constructs it from SUPER_ADMIN_EMAIL; a nil value would crash the
	// admin gate, so it's required by the binary.
	Auth       *auth.Auth
	Domain     string
	Port       string
	SystemInfo SystemInfo
	// PreWarmCert, when non-nil, is invoked in a goroutine for each newly-saved
	// custom domain so the autocert manager can issue a Let's Encrypt cert
	// before the first visitor arrives. Set by main when --acme-email is on;
	// left nil in plain-HTTP / dev mode.
	PreWarmCert func(host string)
}

// Server is the wired-up state shared across handlers.
type Server struct {
	store      *store.Store
	build      *build.Service
	events     *events.Tracker
	llm        adkmodel.LLM
	sandbox    *sandbox.Manager
	state      state.Store
	snapshot   *snapshot.Service
	auth       *auth.Auth
	domain     string
	port       string
	tpl        *template.Template
	systemInfo SystemInfo

	// htmlMinifier strips whitespace + comments from HTML on the serve
	// path. Constructed once at startup so we don't re-allocate the
	// internal mimetype table per request.
	htmlMinifier *minify.M

	// domainIndex maps lowercased custom hostnames to the slug that owns
	// them. slugIndex is the set of real slugs in the bucket, used by
	// HostAllowed so autocert won't ask Let's Encrypt for a cert for a
	// scanner-invented hostname like "whm.apps.jtarchie.com" — every miss
	// would otherwise burn a slot in the 50/week per-registered-domain rate
	// limit. ownerIndex maps slug → owner email so role-filtered listings
	// don't need an S3 GET per app. privateIndex flags slugs whose owner
	// has marked them private — the subdomain proxy consults it on every
	// hit so we don't pay an S3 round-trip for the gate. All four are
	// rebuilt from one ListApps call; the same mutex guards them.
	domainMu     sync.RWMutex
	domainIndex  map[string]string
	slugIndex    map[string]bool
	ownerIndex   map[string]string
	privateIndex map[string]bool

	// preWarmCert is the deps callback, captured here so settingsSubmitHandler
	// can fire it without threading the function through every signature.
	preWarmCert func(host string)
}

// fallThroughHosts are hosts that should bypass subdomain proxying and hit
// the main routes.
var fallThroughHosts = map[string]bool{
	"localhost": true,
	"127.0.0.1": true,
	"0.0.0.0":   true,
}

// New constructs the Echo server with all routes mounted. Returns both the
// Echo instance (for serving) and the underlying *Server (so the autocert
// HostPolicy in main can reach HostAllowed without duplicating the dispatch
// logic from subdomainMiddleware).
func New(d Deps) (*echo.Echo, *Server) {
	tpl := template.New("")
	// layout.html defines shared partials (e.g. "head") used by the platform
	// pages below. It must be parsed first so the others can reference its
	// blocks.
	template.Must(tpl.Parse(layoutTemplate))
	for _, t := range []struct{ name, body string }{
		{"landing", landingTemplate},
		{"apps", appsTemplate},
		{"workspace", workspaceTemplate},
		{"manage", manageTemplate},
		{"system", systemTemplate},
		{"toolbar", editToolbarTemplate},
		{"theme_preview_listener", themePreviewListenerTemplate},
		{"selection_listener", selectionListenerTemplate},
		{"visual_edit", visualEditTemplate},
		{"function_edit", functionEditTemplate},
		{"files", filesTemplate},
		{"debug", debugTemplate},
		{"debug_edit", debugEditTemplate},
		{"login", loginTemplate},
		{"register", registerTemplate},
		{"account", accountTemplate},
		{"admin_users", adminUsersTemplate},
		{"error", errorTemplate},
	} {
		template.Must(tpl.New(t.name).Parse(t.body))
	}

	s := &Server{
		store:        d.Store,
		build:        d.Build,
		events:       d.Events,
		llm:          d.LLM,
		sandbox:      d.Sandbox,
		state:        d.State,
		snapshot:     d.Snapshot,
		auth:         d.Auth,
		domain:       d.Domain,
		port:         d.Port,
		tpl:          tpl,
		systemInfo:   d.SystemInfo,
		htmlMinifier: newHTMLMinifier(),
		domainIndex:  map[string]string{},
		slugIndex:    map[string]bool{},
		ownerIndex:   map[string]string{},
		privateIndex: map[string]bool{},
		preWarmCert:  d.PreWarmCert,
	}
	s.initialRebuildDomainIndex(context.Background())

	e := echo.New()
	e.HTTPErrorHandler = s.httpErrorHandler
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	e.Use(slogecho.New(logger))
	e.Use(hstsMiddleware())
	e.Use(s.subdomainMiddleware())

	// /status and /events are session-required and ownership-scoped: the
	// progress page polls them mid-build, but neither carries data a
	// non-owner should be able to subscribe to. Previously they were
	// open by design so the cookie could propagate cross-page; with the
	// passkey session cookie set on first reach to the admin surface,
	// that handoff is no longer a problem.
	e.GET("/status/:slug", s.statusHandler, s.requireUser, s.requireSlugOwnership)
	e.GET("/events/:slug", s.eventsHandler, s.requireUser, s.requireSlugOwnership)
	e.GET("/favicon.svg", s.faviconHandler)

	// Path-based access to a hosted site, mirroring the subdomain route. The
	// subdomain form (slug.<domain>) is canonical and what production uses;
	// /s/:slug is the local-dev fallback for environments where running
	// passkeys against a real domain is awkward (--domain localhost gets you
	// a secure WebAuthn context for free but loses *.lvh.me style
	// subdomains). Same private-site gate via dispatchSite — no information
	// leak between the two surfaces.
	e.Any("/s/:slug", s.pathRouteHandler)
	e.Any("/s/:slug/*", s.pathRouteHandler)

	// Passkey surfaces. Mounted unauthenticated and parallel to the legacy
	// basic-auth admin gate during the rollout; commit 4 swaps requireAdmin
	// over to the session check these endpoints set up. Skipped entirely
	// when SUPER_ADMIN_EMAIL was empty, so dev deployments stay on the
	// single-tenant path.
	if s.auth != nil {
		authMux := http.NewServeMux()
		s.auth.Passkey.MountRoutes(authMux, "/auth/")
		e.Any("/auth/*", echo.WrapHandler(authMux))
		e.GET("/login", s.loginHandler)
		e.GET("/register", s.registerHandler)
		e.POST("/register/finish", s.registerFinishHandler)
		e.POST("/logout", s.logoutHandler)
	}

	admin := e.Group("", s.requireUser)
	// promptBodyCap bounds the whole request body on prompt-bearing POSTs so a
	// runaway hidden field or selection can't sneak past the per-field caps in
	// the handlers. Leaves /upload/:slug alone — image uploads need 5 MiB.
	promptBodyCap := middleware.BodyLimit(maxPromptBodyBytes)
	// promptWithAttachmentsBodyCap is the larger envelope on routes that also
	// accept multipart markdown attachments; per-attachment caps still gate
	// the actual content.
	promptWithAttachmentsBodyCap := middleware.BodyLimit(maxPromptBodyWithAttachmentsBytes)
	admin.GET("/", s.landingHandler)
	admin.POST("/build", s.buildHandler, promptWithAttachmentsBodyCap)
	admin.GET("/apps", s.appsHandler)
	admin.GET("/system", s.systemHandler)
	admin.GET("/account", s.accountHandler)

	// Super-admin-only surfaces. requireSuperAdmin layers role check on
	// top of requireUser, so these routes live outside the regular admin
	// group (which only checks logged-in).
	e.GET("/admin/users", s.adminUsersHandler, s.requireSuperAdmin)
	e.POST("/admin/users/invite", s.adminInviteCreateHandler, s.requireSuperAdmin)
	e.POST("/admin/invites/:token/revoke", s.adminInviteRevokeHandler, s.requireSuperAdmin)
	e.POST("/admin/users/:email/disable", s.adminUserDisableHandler, s.requireSuperAdmin)
	e.POST("/admin/users/:email/enable", s.adminUserEnableHandler, s.requireSuperAdmin)
	e.POST("/admin/users/:email/sessions/revoke", s.adminUserRevokeSessionsHandler, s.requireSuperAdmin)
	e.POST("/admin/users/:email/quotas", s.adminUserQuotasHandler, s.requireSuperAdmin)
	// Per-slug routes carry the ownership gate as route-level middleware
	// so a regular admin gets a 404 on every slug they don't own without
	// each handler having to repeat the check.
	owns := s.requireSlugOwnership
	admin.GET("/workspace/:slug", s.workspaceHandler, owns)
	admin.GET("/manage/:slug", s.manageHandler, owns)
	admin.GET("/edit/:slug", s.redirectToWorkspace, owns)
	admin.POST("/edit/:slug", s.editSubmitHandler, owns, promptWithAttachmentsBodyCap)
	admin.POST("/relint/:slug", s.relintHandler, owns)
	admin.GET("/edit/:slug/visual", s.visualEditHandler, owns)
	admin.POST("/edit/:slug/visual", s.visualEditSaveHandler, owns, promptBodyCap)
	admin.GET("/edit/:slug/theme", s.redirectToWorkspace, owns)
	admin.POST("/edit/:slug/theme", s.themeStudioApplyHandler, owns)
	admin.GET("/edit/:slug/function/:name", s.functionEditHandler, owns)
	admin.POST("/test/:slug/api/:name", s.functionTestHandler, owns)
	admin.POST("/upload/:slug", s.uploadHandler, owns)
	admin.GET("/export/:slug", s.exportHandler, owns)
	admin.POST("/import", s.importHandler, middleware.BodyLimit(portable.MaxArchiveBytes+(64*1024)))
	admin.POST("/files/:slug/delete", s.deleteFileHandler, owns)
	admin.POST("/files/:slug/rename", s.renameFileHandler, owns)
	admin.GET("/settings/:slug", s.redirectToManage, owns)
	admin.POST("/settings/:slug", s.settingsSubmitHandler, owns)
	admin.POST("/settings/:slug/delete", s.settingsDeleteHandler, owns)
	admin.POST("/manage/:slug/remix", s.remixHandler, owns)
	admin.POST("/apps/:slug/transfer", s.transferAppHandler, owns)
	admin.GET("/history/:slug", s.redirectToWorkspace, owns)
	admin.POST("/history/:slug/restore", s.historyRestoreHandler, owns)
	admin.POST("/history/:slug/delete", s.historyDeleteHandler, owns)
	admin.GET("/data/:slug", s.dataHandler, owns)
	admin.GET("/files/:slug", s.filesHandler, owns)
	admin.GET("/debug/:slug", s.debugHandler, owns)
	admin.GET("/debug/:slug/edit", s.debugDetailHandler, owns)
	admin.GET("/debug/:slug/cache-check", s.debugCacheCheckHandler, owns)

	return e, s
}

// subdomainMiddleware dispatches by Host:
//
//  1. main domain (or loopback) → admin routes (gated by requireAdmin).
//  2. `*.<domain>` subdomain    → proxy/api for that slug.
//  3. registered custom domain  → proxy/api for the owning slug, with the
//     custom-domain flag set so cache headers go public and the toolbar
//     stays hidden.
//  4. anything else             → 404 (don't let unknown Host headers fall
//     through to admin routes — that's the leak we're closing).
//
// Path-based dispatch inside cases 2 and 3:
//  1. /api/{name}  → apiHandler (only when the template enabled functions)
//  2. anything else → proxyHandler (static)
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			host := stripPort(c.Request().Host)

			if host == s.domain || fallThroughHosts[host] {
				return next(c)
			}

			if slug, ok := strings.CutSuffix(host, "."+s.domain); ok {
				// Same three-gate check as HostAllowed: reject nested
				// subdomains, invalid slug shape, and slugs that don't
				// correspond to a real app. Stops scanner traffic from
				// generating per-request log noise and from waking up the S3
				// lookup path.
				if strings.Contains(slug, ".") || validateSlug(slug) != nil || !s.slugExists(slug) {
					return notFound()
				}
				return s.dispatchSite(c, slug)
			}

			if slug, ok := s.lookupCustomDomain(host); ok {
				c.Set("custom_domain", true)
				return s.dispatchSite(c, slug)
			}

			return notFound()
		}
	}
}

// pathRouteHandler serves a hosted site via /s/:slug[/...], reusing the
// dispatchSite path that the subdomain route also funnels through. Slug
// validation mirrors subdomainMiddleware so an unknown or malformed slug
// returns 404 instead of leaking the existence of admin routes. The URL
// path is rewritten in place so proxyHandler / apiHandler see the relative
// site path (e.g. /about.html, /api/submit), exactly as they would on the
// subdomain.
func (s *Server) pathRouteHandler(c *echo.Context) error {
	slug := c.Param("slug")
	if validateSlug(slug) != nil || !s.slugExists(slug) {
		return notFound()
	}

	req := c.Request()
	rest := strings.TrimPrefix(req.URL.Path, "/s/"+slug)
	if rest == "" {
		rest = "/"
	}
	req.URL.Path = rest

	return s.dispatchSite(c, slug)
}

// dispatchSite routes a request that's already been mapped to a slug to either
// /api or the static proxy. Private slugs are gated here — anyone but the
// owner (or a super admin) gets a 404 so the existence of a private site
// can't be inferred from the status code.
func (s *Server) dispatchSite(c *echo.Context, slug string) error {
	if s.isPrivate(slug) && !s.callerCanViewPrivate(c, slug) {
		return notFound()
	}
	reqPath := c.Request().URL.Path
	if name, ok := strings.CutPrefix(reqPath, "/api/"); ok {
		return s.apiHandler(c, slug, name)
	}
	return s.proxyHandler(c, slug)
}

// callerCanViewPrivate answers whether the request's session belongs to a
// user permitted to see a private site. The subdomain path doesn't go
// through requireUser so we read the session cookie directly — the same
// cookie the admin chain uses, just resolved inline without erroring on
// miss. Super admins always pass; otherwise the email must match the
// recorded owner.
func (s *Server) callerCanViewPrivate(c *echo.Context, slug string) bool {
	email, ok := s.currentSessionEmail(c)
	if !ok {
		return false
	}
	if email == s.ownerOf(slug) {
		return true
	}
	if s.auth == nil {
		return false
	}
	u, err := s.auth.Users.LookupCached(c.Request().Context(), email)
	if err != nil {
		return false
	}
	return u.Role == auth.RoleSuperAdmin
}

// rebuildDomainIndex scans all sites and rebuilds the host → slug map. Called
// after any settings save that changes Domains. Returns an error so the
// initial startup rebuild can retry; runtime callers (settings handlers) just
// log and continue — a stale index there only delays the next refresh.
func (s *Server) rebuildDomainIndex(ctx context.Context) error {
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	idx := make(map[string]string, len(apps))
	slugs := make(map[string]bool, len(apps))
	owners := make(map[string]string, len(apps))
	privates := make(map[string]bool, len(apps))
	for _, slug := range apps {
		slugs[slug] = true
		meta := s.build.ReadMeta(ctx, slug)
		if meta.OwnerID != "" {
			owners[slug] = meta.OwnerID
		}
		if meta.Private {
			privates[slug] = true
		}
		for _, d := range meta.Domains {
			if existing, dup := idx[d]; dup && existing != slug {
				slog.Warn("domain_index.duplicate", "domain", d, "kept", existing, "dropped", slug)
				continue
			}
			idx[d] = slug
		}
	}
	s.domainMu.Lock()
	s.domainIndex = idx
	s.slugIndex = slugs
	s.ownerIndex = owners
	s.privateIndex = privates
	s.domainMu.Unlock()
	slog.Info("domain_index.rebuilt", "domains", len(idx), "slugs", len(slugs), "owners", len(owners), "private", len(privates))
	return nil
}

// ownerOf returns the owner email recorded in the index for a slug, or
// the empty string if the slug is unowned (pre-migration data). Cheap —
// one in-memory map lookup. The caller is expected to handle "":
// authorizeSlug treats it as "only super admin can access."
func (s *Server) ownerOf(slug string) string {
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	return s.ownerIndex[slug]
}

// isPrivate reports whether the slug is marked private. Consulted by the
// subdomain dispatcher on every public-facing hit so we can gate the
// proxy without an extra S3 round-trip per request.
func (s *Server) isPrivate(slug string) bool {
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	return s.privateIndex[slug]
}

// setOwner refreshes a single slug's owner without rebuilding the whole
// index. Called from buildHandler (after a new app is created) and from
// the transfer handler in commit 8.
func (s *Server) setOwner(slug, owner string) {
	s.domainMu.Lock()
	if s.ownerIndex == nil {
		s.ownerIndex = map[string]string{}
	}
	s.ownerIndex[slug] = owner
	s.domainMu.Unlock()
}

// countAppsFor returns the number of slugs the given email owns according
// to the in-memory ownerIndex. Used by the quota check on /build and by
// the over-quota banner on /apps. Empty email returns 0.
func (s *Server) countAppsFor(email string) int {
	if email == "" {
		return 0
	}
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	count := 0
	for _, owner := range s.ownerIndex {
		if owner == email {
			count++
		}
	}
	return count
}

// markSlug records a freshly-created slug so HostAllowed accepts it
// immediately, without waiting for the next ListApps rebuild. Called from
// buildHandler the moment a build is kicked off — the slug folder may not
// exist in S3 yet, but the user is already redirected to its URL and we want
// the first TLS handshake to succeed.
func (s *Server) markSlug(slug string) {
	s.domainMu.Lock()
	if s.slugIndex == nil {
		s.slugIndex = map[string]bool{}
	}
	s.slugIndex[slug] = true
	s.domainMu.Unlock()
}

// slugExists reports whether slug names a real app in our index. Used by
// HostAllowed to refuse ACME issuance for scanner-invented hostnames.
func (s *Server) slugExists(slug string) bool {
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	return s.slugIndex[slug]
}

// initialRebuildDomainIndex retries the first rebuild a few times. If S3 is
// briefly unreachable at boot and we silently start with an empty index, every
// custom-domain ACME validation fails closed (HostPolicy denies unknown hosts)
// until somebody saves settings — that's a long, silent outage. Keep retrying
// for ~10s; if the bucket genuinely is dead, panic so the platform restarts us.
func (s *Server) initialRebuildDomainIndex(ctx context.Context) {
	var lastErr error
	for i := range 5 {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		err := s.rebuildDomainIndex(ctx)
		if err == nil {
			return
		}
		lastErr = err
		slog.Warn("domain_index.startup_retry", "attempt", i+1, "err", err)
	}
	panic(fmt.Errorf("initial domain index rebuild failed after retries: %w", lastErr))
}

// rebuildDomainIndexLogging is the post-startup callsite: rebuild, log on
// failure, keep serving. The old index stays in place if the rebuild errored.
func (s *Server) rebuildDomainIndexLogging(ctx context.Context) {
	err := s.rebuildDomainIndex(ctx)
	if err != nil {
		slog.Warn("domain_index.refresh_failed", "err", err)
	}
}

// lookupCustomDomain returns the slug that owns host, if any.
func (s *Server) lookupCustomDomain(host string) (string, bool) {
	s.domainMu.RLock()
	defer s.domainMu.RUnlock()
	slug, ok := s.domainIndex[host]
	return slug, ok
}

// hstsMiddleware advertises HSTS only when the request actually arrived over
// TLS (c.Request().TLS != nil — true on the autocert HTTPS listener, false
// on `task local` plain HTTP). Two years is the long-standing recommendation;
// includeSubDomains covers per-slug subdomains. Preload is intentionally off
// — wait for a few weeks of clean issuance before opting in (it's irrevocable
// for ~12 months).
func hstsMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if c.Request().TLS != nil {
				c.Response().Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			return next(c)
		}
	}
}

// HostAllowed mirrors subdomainMiddleware's dispatch table: the host is
// recognised if it's the main domain (or a loopback fall-through), a *valid*
// slug subdomain (single label that passes validateSlug), or a registered
// custom domain. Exported so the autocert HostPolicy can reuse the same
// source of truth — without the validateSlug check, scanners pummeling us
// with hosts like "whm.whm.x.apps.jtarchie.com" would each trigger a new LE
// issuance attempt and burn the 50/week per-registered-domain rate limit.
func (s *Server) HostAllowed(host string) bool {
	host = strings.ToLower(stripPort(host))
	if host == s.domain || fallThroughHosts[host] {
		return true
	}
	if prefix, ok := strings.CutSuffix(host, "."+s.domain); ok {
		// Three gates in order of cost: cheap shape check first (rejects
		// nested subdomains), then validateSlug (cheap), then the slug
		// existence check (in-memory map lookup) — anything past the shape
		// gate that doesn't name a real app gets refused before autocert
		// ever calls Let's Encrypt. Without this last gate, a scanner
		// hitting "foo.apps.jtarchie.com" triggers a real LE issuance and
		// burns a slot in the 50/week per-registered-domain rate limit.
		if strings.Contains(prefix, ".") {
			return false
		}
		if validateSlug(prefix) != nil {
			return false
		}
		return s.slugExists(prefix)
	}
	_, ok := s.lookupCustomDomain(host)
	return ok
}

// publicPort extracts the port the visitor used from the request's Host
// header. Empty when the request was on a default port (80/443) so we can
// build URLs without a trailing :443. Falls back to s.port for safety.
func (s *Server) publicPort(c *echo.Context) string {
	if i := strings.LastIndex(c.Request().Host, ":"); i != -1 {
		return c.Request().Host[i:] // includes the leading ':'
	}
	return ""
}

// siteURL builds the public URL of a hosted site, using the same scheme/port
// the admin caller is on. https://slug.apps.jtarchie.com on Fly,
// http://slug.localhost:8080 in dev.
func (s *Server) siteURL(c *echo.Context, slug, path string) string {
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.Scheme() + "://" + slug + "." + s.domain + s.publicPort(c) + path
}

// faviconHandler serves the embedded Top Banana banana mark on the admin
// host. Subdomain requests never reach here — subdomainMiddleware intercepts
// them and proxies to S3, so user sites get their own favicon (or 404).
func (s *Server) faviconHandler(c *echo.Context) error {
	c.Response().Header().Set("Cache-Control", "public, max-age=86400")
	return c.Blob(http.StatusOK, "image/svg+xml", []byte(faviconSVG)) //nolint:wrapcheck
}

// adminURL builds an absolute URL on the main app domain. Used by the toolbar
// links injected into hosted-site pages on subdomains, where relative paths
// would point at the wrong host.
func (s *Server) adminURL(c *echo.Context, path string) string {
	if path != "" && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.Scheme() + "://" + s.domain + s.publicPort(c) + path
}

// snapshotBefore wraps snapshot.Create with the "warn-only" policy used by
// every non-build mutation hook. Losing an undo point is not as bad as
// blocking the user's edit, so failures are logged and swallowed.
func (s *Server) snapshotBefore(ctx context.Context, slug, reason string) {
	if s.snapshot == nil {
		return
	}
	_, err := s.snapshot.Create(ctx, slug, reason)
	if err != nil {
		slog.Warn("snapshot.create_failed", "slug", slug, "reason", reason, "err", err)
	}
}

// effectiveTiersFor returns the per-user tier overrides to thread into
// build.Params. Auth-disabled deployments hand back nil — build.Service
// then falls through to its operator-configured TierMap for every tier.
// With auth on, the user's AllowedModels overrides are layered on the
// operator defaults so build.Service sees one merged map per request.
func (s *Server) effectiveTiersFor(user *auth.User) model.TierMap {
	if s.auth == nil {
		return nil
	}
	return auth.ResolveTiers(user, s.auth.QuotaDefaults())
}

// startBuild kicks off the build via the build service and lands the user
// in the workspace with ?building=1, which makes the workspace render its
// inline status strip and open the /events/:slug SSE stream. SSE subscribers
// learn about the build through the events tracker.
func (s *Server) startBuild(c *echo.Context, p build.Params) error {
	s.build.Start(p)
	return c.Redirect(http.StatusSeeOther, "/workspace/"+p.Slug+"?building=1") //nolint:wrapcheck
}

// redirectToWorkspace is the GET handler for legacy /edit/:slug,
// /edit/:slug/theme, and /history/:slug paths. Their content all lives inside
// the workspace now — as the main editor for /edit, and as side panels for
// theme + history.
func (s *Server) redirectToWorkspace(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.Redirect(http.StatusFound, "/workspace/"+slug) //nolint:wrapcheck
}

// redirectToManage is the GET handler for legacy /settings/:slug. Manage
// replaces settings and folds in the data table + advanced links + danger
// zone.
func (s *Server) redirectToManage(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	return c.Redirect(http.StatusFound, "/manage/"+slug) //nolint:wrapcheck
}

func (s *Server) render(c *echo.Context, name string, data any) error {
	data = s.injectChrome(c, data)
	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, name, data)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "render "+name, err)
	}
	return c.HTML(http.StatusOK, buf.String()) //nolint:wrapcheck
}

// injectChrome layers session-derived chrome values onto the page data
// so the shared brand partial renders consistently regardless of which
// handler produced the struct. Data structs embed Chrome by value, so
// passing a pointer to the struct lets us reach the embedded Chrome via
// the chromed interface and set IsSuperAdmin.
//
// Handlers that pass a struct value get rewrapped in a pointer so the
// embedded Chrome is settable. Anything that doesn't satisfy chromed
// passes through unchanged — useful for templates whose page-data type
// genuinely doesn't render the brand partial.
func (s *Server) injectChrome(c *echo.Context, data any) any {
	isSuper := false
	u := userFromContext(c)
	if u != nil {
		isSuper = u.Role == auth.RoleSuperAdmin
	}
	// Struct values are not addressable through the chromed interface;
	// wrap in a pointer so the embedded Chrome can be set in place.
	if data != nil {
		v := reflect.ValueOf(data)
		if v.Kind() == reflect.Struct {
			ptr := reflect.New(v.Type())
			ptr.Elem().Set(v)
			data = ptr.Interface()
		}
	}
	if ch, ok := data.(chromed); ok {
		ch.chromePtr().IsSuperAdmin = isSuper
	}
	return data
}

func httpErr(code int, msg string, err error) *echo.HTTPError {
	return echo.NewHTTPError(code, fmt.Sprintf("%s: %s", msg, err))
}

// notFound returns a real *echo.HTTPError so it survives the slog-echo
// middleware unchanged. The echo.Err* sentinels (echo.ErrNotFound &c.) are an
// unexported *httpError type that slog-echo's `err.(*echo.HTTPError)` check
// fails, after which slog-echo wraps them in a fresh 500 — turning every 404
// into an Internal Server Error in the response. Use this helper anywhere
// we'd reach for echo.ErrNotFound.
func notFound() *echo.HTTPError {
	return echo.NewHTTPError(http.StatusNotFound, http.StatusText(http.StatusNotFound))
}

// errorData backs templates/error.html. The monkey page is intentionally
// chrome-less — it works on subdomains where there's no logged-in user as
// well as on admin pages where we'd rather not lean on injectChrome to
// re-fetch session state during an error response.
type errorData struct {
	Status  int
	Title   string
	Tagline string
}

// httpErrorHandler replaces Echo's default JSON serializer. Browser
// navigations (Accept: text/html) get the monkey page; everything else —
// fetch() with default Accept, the SSE poller, /api/* function callers,
// curl — keeps the original {"message": "..."} JSON so no client breaks.
func (s *Server) httpErrorHandler(c *echo.Context, err error) {
	if r, _ := echo.UnwrapResponse(c.Response()); r != nil && r.Committed {
		return
	}
	code := http.StatusInternalServerError
	msg := http.StatusText(code)
	var he *echo.HTTPError
	if errors.As(err, &he) {
		code = he.Code
		if he.Message != "" {
			msg = he.Message
		}
	}

	if !wantsHTML(c) {
		_ = c.JSON(code, map[string]string{"message": msg})
		return
	}

	var buf bytes.Buffer
	rErr := s.tpl.ExecuteTemplate(&buf, "error", errorData{
		Status:  code,
		Title:   "This bunch is missing a banana.",
		Tagline: "We swung through every vine and peeled every page — no luck. Try heading home and we'll start fresh.",
	})
	if rErr != nil {
		_ = c.String(code, msg)
		return
	}
	_ = c.HTML(code, buf.String())
}

// wantsHTML returns true for plain browser navigations. fetch(), curl, and
// SSE clients leave Accept unset or send `*/*`, which we treat as JSON to
// preserve the existing API contract.
func wantsHTML(c *echo.Context) bool {
	return strings.Contains(c.Request().Header.Get("Accept"), "text/html")
}

// landingData backs templates/landing.html. Was a map[string]any until
// the chrome refactor; the typed struct lets the shared brand partial
// pick up IsSuperAdmin via embedded promotion.
type landingData struct {
	Chrome
	Templates []*templates.SiteTemplate
	Domain    string
}

func (s *Server) landingHandler(c *echo.Context) error {
	return s.render(c, "landing", landingData{
		Templates: templates.All(),
		Domain:    s.domain,
	})
}

type appLink struct {
	Name        string
	Title       string
	Description string
	URL         string
	// LastEdited is a human-readable relative timestamp ("3m ago", "yesterday")
	// derived from the most recent transcript in editrec. Empty when there's no
	// edit history yet — the card omits the line in that case.
	LastEdited string
	// PrimaryDomain is the first entry in meta.Domains, or "" when no custom
	// domain is configured. Surfaced on the row next to the slug so the user
	// can see at a glance which apps live at their own address.
	PrimaryDomain string
}

type appsData struct {
	Chrome
	Apps  []appLink
	Flash string
	// OverQuotaCount is the diff between the current owned-app count and
	// the user's MaxApps cap, or 0 when within quota. Triggers the banner
	// at the top of the listing.
	OverQuotaCount int
	MaxApps        int
}

func (s *Server) appsHandler(c *echo.Context) error {
	ctx := c.Request().Context()
	user := userFromContext(c)
	apps, err := s.store.ListApps(ctx)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list apps", err)
	}

	links := make([]appLink, 0, len(apps))
	for _, app := range apps {
		meta := s.build.ReadMeta(ctx, app)
		// Role-filter: regular admins only see their own apps. Super
		// admin sees everything regardless. Pre-migration data with an
		// empty OwnerID falls through to super-admin-only on purpose;
		// the startup migration assigns those on every boot.
		if user != nil && user.Role != auth.RoleSuperAdmin && meta.OwnerID != user.Email {
			continue
		}
		primaryDomain := ""
		if len(meta.Domains) > 0 {
			primaryDomain = meta.Domains[0]
		}
		links = append(links, appLink{
			Name:          app,
			Title:         meta.Title,
			Description:   meta.Description,
			URL:           s.siteURL(c, app, "/"),
			LastEdited:    lastEditedFor(ctx, s, app),
			PrimaryDomain: primaryDomain,
		})
	}
	sort.SliceStable(links, func(i, j int) bool {
		return appLinkKey(links[i]) < appLinkKey(links[j])
	})

	over, cap := s.appsOverQuota(user)
	return s.render(c, "apps", appsData{
		Chrome:         Chrome{Active: "apps"},
		Apps:           links,
		Flash:          c.QueryParam("flash"),
		OverQuotaCount: over,
		MaxApps:        cap,
	})
}

// appsOverQuota returns (overByN, effectiveCap) for the given user.
// overByN > 0 means the user is past their cap (almost always because
// they were transferred into it) and the listing should render a
// banner. Super admins are never over quota.
func (s *Server) appsOverQuota(user *auth.User) (int, int) {
	if s.auth == nil || user == nil || user.Role == auth.RoleSuperAdmin {
		return 0, 0
	}
	defaults := s.auth.QuotaDefaults()
	cap := user.Quotas.MaxApps
	if cap == 0 {
		cap = defaults.MaxApps
	}
	if cap <= 0 {
		return 0, 0
	}
	count := s.countAppsFor(user.Email)
	if count <= cap {
		return 0, cap
	}
	return count - cap, cap
}

// siteNameOrSlug returns the site's friendly title for the chrome breadcrumb,
// falling back to the slug itself when no title has been generated yet. Used
// by the per-site handlers that share layout.html's `brand` partial.
func (s *Server) siteNameOrSlug(ctx context.Context, slug string) string {
	meta := s.build.ReadMeta(ctx, slug)
	if meta.Title != "" {
		return meta.Title
	}
	return slug
}

// lastEditedFor returns a relative timestamp for the most recent transcript,
// or "" when the site has no edits recorded yet (a freshly-created shell with
// no completed build, or a pre-editrec site). The transcript list is small
// (capped by retention in editrec.Trim), so this is O(N) per app card.
func lastEditedFor(ctx context.Context, s *Server, slug string) string {
	rows, err := editrec.List(ctx, s.store, slug)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return humanizeAge(rows[0].Timestamp)
}

// appLinkKey orders apps by Title when present, otherwise by slug — keeps
// the listing readable as titles fill in over time without burying legacy
// (title-less) sites at the end.
func appLinkKey(a appLink) string {
	if a.Title != "" {
		return strings.ToLower(a.Title)
	}
	return strings.ToLower(a.Name)
}

// maxPromptBytes caps the user-supplied prompt on /build, /edit/:slug, and
// /edit/:slug/visual. Most real prompts are under a few hundred bytes; this is
// the field-level check that pairs with maxPromptBodyBytes on the route.
const maxPromptBytes = 4 * 1024

// maxPromptBodyBytes caps the entire request body on prompt-bearing POSTs.
// The handler-side check on maxPromptBytes still applies; this bounds the
// overall body so the hidden selection field and any other form data combined
// can't blow past a sane budget.
const maxPromptBodyBytes = 32 * 1024

// maxPromptBodyWithAttachmentsBytes is the outer envelope on /build and
// /edit/:slug where users can attach markdown files alongside the prompt:
// 4 KiB prompt + 512 KiB total attachments + multipart overhead headroom. The
// per-field and per-attachment caps below still gate the actual content.
const maxPromptBodyWithAttachmentsBytes = 768 * 1024

const (
	// maxAttachmentBytes caps each individual markdown attachment. Picked to
	// keep the model's context window manageable when several files are
	// inlined at once.
	maxAttachmentBytes = 64 * 1024
	// maxAttachments caps how many markdown files can ride a single request.
	maxAttachments = 10
	// maxAttachmentsTotalBytes caps the combined size; pairs with the per-file
	// cap so the worst case stays bounded for a small file count too.
	maxAttachmentsTotalBytes = 512 * 1024
)

// parseAttachments pulls user-uploaded reference files (markdown or HTML)
// out of a multipart form on /build and /edit/:slug. Returns nil for "no
// files attached" (which is the common case — the input is optional).
// Validation failures surface as 400s with a readable message; nothing is
// silently dropped.
func parseAttachments(c *echo.Context) ([]agent.Attachment, error) {
	form, err := c.MultipartForm()
	if err != nil {
		// Not a multipart submission (URL-encoded form): treat as "no attachments".
		if errors.Is(err, http.ErrNotMultipart) {
			return nil, nil
		}
		return nil, echo.NewHTTPError(http.StatusBadRequest, "could not parse upload: "+err.Error())
	}
	if form == nil {
		return nil, nil
	}
	files := form.File["attachment"]
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > maxAttachments {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("too many attachments (max %d)", maxAttachments))
	}

	out := make([]agent.Attachment, 0, len(files))
	seen := make(map[string]int, len(files))
	total := 0
	for _, fh := range files {
		if fh.Size > maxAttachmentBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is too large (max %d bytes)", fh.Filename, maxAttachmentBytes))
		}
		name, err := sanitizeAttachmentName(fh.Filename)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		f, err := fh.Open()
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("open attachment %q: %s", fh.Filename, err.Error()))
		}
		body, readErr := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
		_ = f.Close()
		if readErr != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("read attachment %q: %s", fh.Filename, readErr.Error()))
		}
		if len(body) > maxAttachmentBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is too large (max %d bytes)", fh.Filename, maxAttachmentBytes))
		}
		if !utf8.Valid(body) {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is not valid UTF-8 text", fh.Filename))
		}
		total += len(body)
		if total > maxAttachmentsTotalBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachments exceed combined size limit (%d bytes)", maxAttachmentsTotalBytes))
		}
		// On duplicate basenames, suffix -2, -3, ... so the agent's seed loop
		// still gets distinct lookup keys.
		uniq := name
		if n := seen[name]; n > 0 {
			ext := path.Ext(name)
			stem := strings.TrimSuffix(name, ext)
			uniq = fmt.Sprintf("%s-%d%s", stem, n+1, ext)
		}
		seen[name]++
		out = append(out, agent.Attachment{Name: uniq, Content: string(body)})
	}
	return out, nil
}

// allowedAttachmentExts are the file extensions accepted for reference
// attachments. Markdown for prose source, HTML for existing pages the user
// wants the agent to draw from.
var allowedAttachmentExts = []string{".md", ".markdown", ".html", ".htm"}

// sanitizeAttachmentName returns a safe basename for an uploaded reference
// file. Rejects empty, path-bearing, non-markdown/HTML, or syntactically
// suspicious names. The result is always lowercase and limited to [a-z0-9._-].
func sanitizeAttachmentName(raw string) (string, error) {
	base := path.Base(strings.TrimSpace(raw))
	if base == "" || base == "." || base == "/" {
		return "", errors.New("attachment filename is empty")
	}
	lower := strings.ToLower(base)
	if !hasAnySuffix(lower, allowedAttachmentExts) {
		return "", fmt.Errorf("attachment %q must end in %s", raw, strings.Join(allowedAttachmentExts, ", "))
	}
	if len(lower) > 80 {
		return "", fmt.Errorf("attachment name %q is too long (max 80 chars)", raw)
	}
	if bad, ok := firstDisallowedRune(lower); ok {
		return "", fmt.Errorf("attachment name %q contains unsupported character %q (allowed: a-z, 0-9, . _ -)", raw, bad)
	}
	return lower, nil
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, suf := range suffixes {
		if strings.HasSuffix(s, suf) {
			return true
		}
	}
	return false
}

func firstDisallowedRune(name string) (rune, bool) {
	for _, r := range name {
		if !markdownNameRuneAllowed(r) {
			return r, true
		}
	}
	return 0, false
}

func markdownNameRuneAllowed(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.' || r == '_' || r == '-':
		return true
	}
	return false
}

func (s *Server) buildHandler(c *echo.Context) error {
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("prompt is too long (max %d bytes)", maxPromptBytes))
	}

	attachments, err := parseAttachments(c)
	if err != nil {
		return err
	}

	requested := strings.TrimSpace(c.FormValue("slug"))
	slug, err := s.resolveSlug(c.Request().Context(), requested)
	if err != nil {
		return err
	}

	tmpl := templates.Get(c.FormValue("template"))
	user := userFromContext(c)
	owner := ""
	if user != nil {
		owner = user.Email
	}

	// Quota gates + per-user tier resolution. CheckMaxApps gates the apps
	// cap (super admins bypass). effectiveTiersFor returns the user's
	// AllowedModels merged over the operator defaults so build.Service
	// dispatches each tier against the matching Runner.
	if s.auth != nil {
		quotaErr := auth.CheckMaxApps(user, s.countAppsFor(owner), s.auth.QuotaDefaults())
		if quotaErr != nil {
			return echo.NewHTTPError(http.StatusForbidden, quotaErr.Error())
		}
	}
	tiers := s.effectiveTiersFor(user)

	slog.Info("build.start", "slug", slug, "template", tmpl.ID, "attachments", len(attachments), "owner", owner, "tiers", tiers)
	// Register the slug + its owner before the build kicks off so the very
	// first TLS handshake to <slug>.<domain> (the progress page about to
	// load) passes HostAllowed, and so /events + /status see ownership
	// without waiting for the next ListApps rebuild.
	s.markSlug(slug)
	if owner != "" {
		s.setOwner(slug, owner)
	}
	return s.startBuild(c, build.Params{
		Slug:         slug,
		Prompt:       prompt,
		LogKey:       "build",
		Template:     tmpl,
		SeedSkeleton: true,
		Attachments:  attachments,
		OwnerID:      owner,
		Tiers:        tiers,
	})
}

// resolveSlug returns either a validated user-provided slug or a freshly
// generated one. User-provided slugs are validated for shape and checked for
// collisions in S3.
func (s *Server) resolveSlug(ctx context.Context, requested string) (string, error) {
	if requested == "" {
		return s.allocateSlug(ctx)
	}
	err := validateSlug(requested)
	if err != nil {
		return "", echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	existing, err := s.store.List(ctx, requested)
	if err != nil {
		return "", httpErr(http.StatusInternalServerError, "check slug", err)
	}
	if len(existing) > 0 {
		return "", echo.NewHTTPError(http.StatusConflict, fmt.Sprintf("slug %q is already taken", requested))
	}
	return requested, nil
}

// allocateSlug draws fresh slugs until it finds one that's unused in the
// bucket. The slug space is small enough (~500k combinations) that
// collisions are realistic once a few hundred apps exist, so the loop
// retries rather than returning the first hit. Used by /build's empty-slug
// path and by the remix handler.
func (s *Server) allocateSlug(ctx context.Context) (string, error) {
	const maxAttempts = 16
	for range maxAttempts {
		candidate := newSlug()
		existing, err := s.store.List(ctx, candidate)
		if err != nil {
			return "", httpErr(http.StatusInternalServerError, "check slug", err)
		}
		if len(existing) == 0 {
			return candidate, nil
		}
	}
	return "", echo.NewHTTPError(http.StatusInternalServerError, "could not allocate a free slug")
}

func (s *Server) statusHandler(c *echo.Context) error {
	slug := c.Param("slug")
	status := s.events.Get(slug)
	if status == nil {
		status = &events.Status{Slug: slug, Status: "unknown"}
	}
	return c.JSON(http.StatusOK, status) //nolint:wrapcheck
}

// eventsHandler streams a slug's build events as SSE. It first replays any
// past events so a late connection still sees what happened, then forwards
// live events until the build hits a terminal status or the client
// disconnects. Each frame carries an `id:` set to the event's index in the
// slug's history; on reconnect EventSource sends Last-Event-ID back and we
// skip past frames the client already rendered — otherwise the page shows
// the entire history twice every time the SSE connection blips.
func (s *Server) eventsHandler(c *echo.Context) error {
	slug := c.Param("slug")
	w := c.Response()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	lastID := parseLastEventID(c.Request().Header.Get("Last-Event-ID"))

	history, sub, terminal := s.events.Subscribe(slug)
	if sub == nil {
		// No status known for this slug — emit a single "unknown" frame and bail.
		_ = writeSSE(w, events.Event{Type: events.TypeStatus, Status: "unknown", Time: time.Now()})
		flush()
		return nil
	}
	defer s.events.Unsubscribe(slug, sub)

	for i, e := range history {
		if i <= lastID {
			continue
		}
		err := writeSSEWithID(w, i, e)
		if err != nil {
			return nil //nolint:nilerr // client gone, just stop streaming
		}
	}
	flush()
	if terminal {
		return nil
	}

	// nextID picks up where history left off so live frames continue the
	// same monotonic sequence the client uses for Last-Event-ID.
	streamLiveEvents(c.Request().Context(), w, sub, len(history), flush)
	return nil
}

// parseLastEventID reads the SSE reconnect cursor from the request header.
// Returns -1 when absent or malformed, which makes the caller's "skip
// indices <= lastID" loop replay the entire history.
func parseLastEventID(h string) int {
	if h == "" {
		return -1
	}
	v, err := strconv.Atoi(h)
	if err != nil {
		return -1
	}
	return v
}

// streamLiveEvents pumps live events from sub to w, assigning each one a
// monotonically-increasing SSE id starting at startID so the sequence
// continues from the history that was already replayed. Returns when the
// channel closes, the request context is canceled, the write fails, or
// the build reaches a terminal status.
func streamLiveEvents(ctx context.Context, w io.Writer, sub chan events.Event, startID int, flush func()) {
	nextID := startID
	for {
		select {
		case e, ok := <-sub:
			if !ok {
				return
			}
			err := writeSSEWithID(w, nextID, e)
			if err != nil {
				return
			}
			nextID++
			flush()
			if e.Type == events.TypeStatus && (e.Status == events.StatusCompleted || e.Status == events.StatusFailed) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// writeSSE writes a frame without an event id. Reserved for one-shot
// sentinel frames like the "unknown" status that aren't part of the
// history sequence and shouldn't influence the client's Last-Event-ID.
func writeSSE(w io.Writer, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// writeSSEWithID writes a frame stamped with `id: <n>` so EventSource
// records it and echoes the latest one back as Last-Event-ID on reconnect.
func writeSSEWithID(w io.Writer, id int, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, payload)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// reservedProxyPrefixes are bucket paths the static proxy must never serve.
// Slugs themselves can't start with "_" (validateSlug), so these only apply
// to paths *within* a real slug — e.g. blocking GET /_state/data.json from
// leaking persisted form data on a site at slug.example.com.
var reservedProxyPrefixes = []string{"_state/", ".topbanana/", ".bloomhollow/", ".buildabear/"}

// reservedProxyPaths are exact bucket paths the static proxy must never
// serve. `.topbanana.json` is the per-site metadata sidecar; `.bloomhollow.json`
// and `.buildabear.json` are pre-rebrand names kept reserved so legacy sites
// can't leak metadata if the new file is missing.
var reservedProxyPaths = map[string]bool{
	".topbanana.json":   true,
	".bloomhollow.json": true,
	".buildabear.json":  true,
}

func (s *Server) proxyHandler(c *echo.Context, slug string) error {
	ctx := c.Request().Context()

	reqPath := strings.TrimPrefix(c.Request().URL.Path, "/")
	if reqPath == "" {
		reqPath = "index.html"
	}

	// Reject traversal *before* the reserved-prefix check — otherwise a path
	// like `assets/../_state/data.json` slips past HasPrefix("_state/").
	if isTraversal(reqPath) || reservedProxyPaths[reqPath] {
		return notFound()
	}
	for _, pfx := range reservedProxyPrefixes {
		if strings.HasPrefix(reqPath, pfx) {
			return notFound()
		}
	}

	candidates := []string{reqPath}
	if !strings.HasSuffix(reqPath, ".html") {
		candidates = append(candidates, reqPath+".html", reqPath+"/index.html")
	}

	for _, candidate := range candidates {
		obj, err := s.store.Read(ctx, slug, candidate)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "read object", err)
		}
		if obj.Content == "" {
			continue
		}

		c.Response().Header().Set("ETag", obj.ETag)
		setProxyCacheHeaders(c)

		if c.Request().Header.Get("If-None-Match") == obj.ETag {
			return c.NoContent(http.StatusNotModified) //nolint:wrapcheck
		}
		if match := c.Request().Header.Get("If-Match"); match != "" && match != obj.ETag {
			return c.NoContent(http.StatusPreconditionFailed) //nolint:wrapcheck
		}

		ct := resolveContentType(obj.ContentType, candidate)
		if strings.HasPrefix(ct, "text/html") {
			body := s.injectEditToolbar(c, obj.Content, slug, candidate)
			minified, mErr := minifyHTMLBody(s.htmlMinifier, body)
			if mErr != nil {
				slog.Warn("serve.minify_failed", "slug", slug, "path", candidate, "err", mErr)
			}
			return c.HTML(http.StatusOK, minified) //nolint:wrapcheck
		}
		return c.Blob(http.StatusOK, ct, []byte(obj.Content)) //nolint:wrapcheck
	}

	return notFound()
}

// setProxyCacheHeaders picks cache headers for static-proxy responses based
// on whether we're serving the main app subdomain (admin previewing — always
// fresh) or a custom domain (cacheable, since a CDN sits in front).
func setProxyCacheHeaders(c *echo.Context) {
	h := c.Response().Header()
	if c.Get("custom_domain") == true {
		h.Set("Cache-Control", "public, max-age=300, s-maxage=3600")
		h.Set("Vary", "Accept-Encoding")
		return
	}
	h.Set("Cache-Control", "no-store")
}

// resolveContentType prefers the type recorded with the object. When that's
// missing or the legacy default (every pre-asset upload was stored as
// text/html), fall back to detecting from the file extension. Older sites
// have all files stamped text/html in S3, so the extension is the only
// signal for assets that were written via the agent's write_file tool —
// those are always HTML, so the default still works.
func resolveContentType(stored, name string) string {
	if stored != "" && stored != store.DefaultContentType {
		return stored
	}
	if ext := path.Ext(name); ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	if stored != "" {
		return stored
	}
	return store.DefaultContentType
}

// injectEditToolbar inserts the theme-preview listener, the selection
// bridge, and (for owners) the edit toolbar before </body>. Skipped
// entirely on custom-domain responses so the CDN never caches admin bytes.
// On the platform domain both listeners always ship — the workspace iframe
// needs them to drive live theme preview and to mark drag-selected copy as
// agent context. They load without the admin session cookie because the
// cookie isn't scoped to subdomains. Each listener is a no-op without a
// postMessage opener (the selection bridge bails when window.parent ===
// window), so direct visitors see no behavior change. The visible toolbar
// (edit links) stays gated on canEdit, since that does leak ownership.
// Returns the content unchanged when there's no </body> to splice into.
func (s *Server) injectEditToolbar(c *echo.Context, htmlContent, slug, page string) string {
	if c.Get("custom_domain") == true {
		return htmlContent
	}
	if !strings.Contains(htmlContent, "</body>") {
		return htmlContent
	}

	var buf bytes.Buffer
	err := s.tpl.ExecuteTemplate(&buf, "theme_preview_listener", nil)
	if err != nil {
		slog.Warn("theme_preview_listener.render_failed", "slug", slug, "err", err)
		return htmlContent
	}

	err = s.tpl.ExecuteTemplate(&buf, "selection_listener", nil)
	if err != nil {
		slog.Warn("selection_listener.render_failed", "slug", slug, "err", err)
		return htmlContent
	}

	if s.canEdit(c, slug) {
		q := url.Values{"page": []string{page}}.Encode()
		editURL := s.adminURL(c, "/edit/"+slug) + "?" + q
		visualURL := s.adminURL(c, "/edit/"+slug+"/visual") + "?" + q

		err := s.tpl.ExecuteTemplate(&buf, "toolbar", struct {
			EditURL   template.URL
			VisualURL template.URL
		}{
			EditURL:   template.URL(editURL),   //nolint:gosec // URL built from controlled inputs above.
			VisualURL: template.URL(visualURL), //nolint:gosec // URL built from controlled inputs above.
		})
		if err != nil {
			slog.Warn("toolbar.render_failed", "slug", slug, "err", err)
			return htmlContent
		}
	} else {
		// canEdit-less branch still needs a </body> in the spliced payload
		// so the document closes properly after the listener.
		buf.WriteString("</body>")
	}

	return strings.Replace(htmlContent, "</body>", buf.String(), 1)
}

// editAsset is the per-image row rendered in the workspace's image library.
// Alt is shown next to the path so users can see what the captioner inferred
// without round-tripping through the agent.
type editAsset struct {
	Path string
	Alt  string
}

func validatePage(page string) error {
	if page == "" {
		return nil
	}
	if isTraversal(page) {
		return fmt.Errorf("invalid page %q", page)
	}
	return nil
}

// isTraversal reports whether a user-supplied object path attempts to escape
// its slug prefix. Rejects `..` segments, absolute paths, and Windows
// separators. Used by both the static proxy and validatePage so a single
// rule covers every code path that reaches the storage layer.
func isTraversal(p string) bool {
	return strings.Contains(p, "..") || strings.HasPrefix(p, "/") || strings.Contains(p, `\`)
}

// reservedSlugs collide with platform routes/hosts and cannot be used as
// site slugs.
var reservedSlugs = map[string]bool{
	"www": true, "api": true, "edit": true, "apps": true,
	"status": true, "build": true, "events": true, "upload": true,
	"history": true, "settings": true, "test": true, "relint": true,
	"admin": true, "login": true, "logout": true,
	"workspace": true, "manage": true, "system": true,
	// Reserved for the multi-tenancy auth surface and ACME challenge paths.
	"auth": true, "webauthn": true, "acme": true, "well-known": true,
	"register": true, "account": true,
}

func validateSlug(slug string) error {
	if len(slug) < 3 || len(slug) > 30 {
		return errors.New("slug must be 3-30 characters")
	}
	for i, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(slug)-1:
		default:
			return errors.New("slug must be lowercase letters, digits, and hyphens (no leading/trailing hyphen)")
		}
	}
	if reservedSlugs[slug] {
		return fmt.Errorf("slug %q is reserved", slug)
	}
	return nil
}

func (s *Server) editSubmitHandler(c *echo.Context) error {
	slug := c.Param("slug")
	prompt := strings.TrimSpace(c.FormValue("prompt"))
	page := c.FormValue("page")
	selection := strings.TrimSpace(c.FormValue("selection"))

	if prompt == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "prompt is required")
	}
	if len(prompt) > maxPromptBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge,
			fmt.Sprintf("prompt is too long (max %d bytes)", maxPromptBytes))
	}
	err := validatePage(page)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	attachments, err := parseAttachments(c)
	if err != nil {
		return err
	}

	if existing := s.events.Get(slug); existing != nil && existing.Status == events.StatusBuilding {
		return echo.NewHTTPError(http.StatusConflict, "edit already in progress for this site")
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	seeds := s.build.EditSeeds(ctx, slug, prompt)
	tiers := s.effectiveTiersFor(userFromContext(c))
	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection), "template", tmpl.ID, "seeds", len(seeds), "attachments", len(attachments), "tiers", tiers)
	return s.startBuild(c, build.Params{
		Slug:         slug,
		Prompt:       build.EditPrompt(prompt, page, selection),
		LogKey:       "edit",
		Template:     tmpl,
		Seeds:        seeds,
		Attachments:  attachments,
		UserPrompt:   prompt,
		Page:         page,
		SelectionLen: len(selection),
		Tiers:        tiers,
	})
}

// relintHandler forces a fresh lint pass and, when issues are found, pushes
// them back to the agent for a fix-up build. On clean sites it redirects to
// the edit page with a flash banner — no LLM cycles spent.
func (s *Server) relintHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	if existing := s.events.Get(slug); existing != nil && existing.Status == events.StatusBuilding {
		return echo.NewHTTPError(http.StatusConflict, "build already in progress for this site")
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	lintErrs := s.build.Lint(ctx, slug, tmpl)

	if len(lintErrs) == 0 {
		slog.Info("relint.clean", "slug", slug)
		return c.Redirect(http.StatusSeeOther, "/workspace/"+slug+"?flash=lint-clean") //nolint:wrapcheck
	}

	// Relint should run entirely on the Editor tier — the prompt is a
	// deterministic lint-fix patch, well within reach of the smaller
	// model that already handles per-build retries. Promoting the
	// resolved Editor model into the Author slot of the override flips
	// every phase of the build over without having to teach build.Service
	// a per-call tier flag.
	resolved := s.effectiveTiersFor(userFromContext(c))
	tiers := model.TierMap{model.TierAuthor: resolved.Resolve(model.TierEditor)}
	slog.Info("relint.start", "slug", slug, "issues", len(lintErrs), "template", tmpl.ID, "tiers", tiers)
	return s.startBuild(c, build.Params{
		Slug:     slug,
		Prompt:   build.LintFixPrompt(lintErrs),
		LogKey:   "relint",
		Template: tmpl,
		Tiers:    tiers,
	})
}

func (s *Server) settingsSubmitHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	ctx := c.Request().Context()
	meta := s.build.ReadMeta(ctx, slug)

	domains, derr := s.parseDomains(c.FormValue("domains"), slug)
	if derr != nil {
		return echo.NewHTTPError(http.StatusBadRequest, derr.Error())
	}
	added := newlyAddedDomains(meta.Domains, domains)
	meta.Domains = domains

	// Only honour the override when the template doesn't already enable
	// functions. Templates that do (contact-form, guestbook, tiny-shop) keep
	// functions on regardless of the per-site bit, so the form's checked-state
	// always matches what the user sees.
	if base := templates.Get(meta.Template); base == nil || !base.EnablesFunctions {
		meta.EnablesFunctions = c.FormValue("enable_functions") == "on"
	}

	meta.EnablesPublicAPI = c.FormValue("enable_public_api") == "on"
	meta.Private = c.FormValue("private") == "on"

	s.snapshotBefore(ctx, slug, snapshot.ReasonSettings)

	err = s.build.WriteMeta(ctx, slug, meta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save settings", err)
	}
	s.rebuildDomainIndexLogging(ctx)
	if s.preWarmCert != nil {
		for _, host := range added {
			go s.preWarmCert(host)
		}
	}
	return c.Redirect(http.StatusSeeOther, "/manage/"+slug) //nolint:wrapcheck
}

// newlyAddedDomains returns the hosts in next that weren't in prev. Both
// lists come from parseDomains so they're already normalized + lowercased.
func newlyAddedDomains(prev, next []string) []string {
	seen := make(map[string]bool, len(prev))
	for _, h := range prev {
		seen[h] = true
	}
	added := make([]string, 0, len(next))
	for _, h := range next {
		if !seen[h] {
			added = append(added, h)
		}
	}
	return added
}

// settingsDeleteHandler permanently removes an app: all site files, all
// snapshots, the in-memory build status, and any custom-domain mapping. The
// caller must POST `confirm` equal to the slug — the typed-slug guard is the
// only safety check.
func (s *Server) settingsDeleteHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if c.FormValue("confirm") != slug {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match slug")
	}

	ctx := c.Request().Context()

	files, err := s.store.List(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list files", err)
	}
	for _, p := range files {
		err = s.store.Delete(ctx, slug, p)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "delete file", err)
		}
	}

	snapCount := 0
	if s.snapshot != nil {
		snaps, err := s.snapshot.List(ctx, slug)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "list snapshots", err)
		}
		for _, sn := range snaps {
			err = s.snapshot.Delete(ctx, slug, sn.Key)
			if err != nil {
				return httpErr(http.StatusInternalServerError, "delete snapshot", err)
			}
		}
		snapCount = len(snaps)
	}

	s.events.Forget(slug)
	s.rebuildDomainIndexLogging(ctx)

	slog.Info("app.delete", "slug", slug, "files", len(files), "snapshots", snapCount)
	return c.Redirect(http.StatusSeeOther, "/apps?flash="+urlEscape("Deleted "+slug)) //nolint:wrapcheck
}

// parseDomains splits the settings-form textarea into a deduped, normalized
// list of hostnames. Rejects entries that collide with the main app domain
// (or its subdomains) and ones already claimed by another slug.
func (s *Server) parseDomains(raw, owningSlug string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, err := build.NormalizeDomain(line)
		if err != nil {
			return nil, fmt.Errorf("normalize domain: %w", err)
		}
		if host == s.domain || strings.HasSuffix(host, "."+s.domain) {
			return nil, fmt.Errorf("domain %q overlaps the main app domain", host)
		}
		if other, ok := s.lookupCustomDomain(host); ok && other != owningSlug {
			return nil, fmt.Errorf("domain %q is already claimed by site %q", host, other)
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out, nil
}

const (
	maxUploadBytes  = 5 << 20 // 5 MiB
	uploadAssetsDir = "assets"
)

// allowedAssetTypes maps sniffed MIME types to a stable file extension we'll
// store under. Keep this restrictive — the agent only knows how to embed
// images via <img>, so we don't accept fonts/video/etc. yet.
var allowedAssetTypes = map[string]string{
	"image/jpeg":    ".jpg",
	"image/png":     ".png",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/svg+xml": ".svg",
}

type uploadResponse struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Alt         string `json:"alt,omitempty"`
	Description string `json:"description,omitempty"`
}

// captionTimeout caps how long the upload handler waits on the vision call
// before giving up and storing the asset without metadata. Local models can
// be slow; we'd rather have a usable upload than a hung POST.
const captionTimeout = 90 * time.Second

func (s *Server) uploadHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	header, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "file is required")
	}
	if header.Size > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	src, err := header.Open()
	if err != nil {
		return httpErr(http.StatusInternalServerError, "open upload", err)
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if len(body) > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	contentType := http.DetectContentType(body)
	contentType = strings.SplitN(contentType, ";", 2)[0]
	ext, ok := allowedAssetTypes[contentType]
	if !ok {
		// SVG sniffs as text/xml or text/plain; trust the extension when the
		// upload looks textual.
		if e := strings.ToLower(path.Ext(header.Filename)); e == ".svg" {
			contentType = "image/svg+xml"
			ext = ".svg"
			ok = true
		}
	}
	if !ok {
		return echo.NewHTTPError(http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported type %q (allowed: jpeg, png, gif, webp, svg)", contentType))
	}

	name, err := safeAssetName(header.Filename, ext)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	relPath := uploadAssetsDir + "/" + name

	// Caption synchronously so the UI can show the suggested alt-text right
	// next to the upload, and so the agent's first list_assets call already
	// has the metadata. Failures here are non-fatal — the upload still lands.
	caption, captionErr := s.captionUpload(c.Request().Context(), body, contentType)
	if captionErr != nil {
		slog.Warn("upload.caption_failed", "slug", slug, "path", relPath, "err", captionErr)
	}

	metadata := map[string]string{}
	if caption.Alt != "" {
		metadata["alt"] = caption.Alt
	}
	if caption.Description != "" {
		metadata["description"] = caption.Description
	}

	s.snapshotBefore(c.Request().Context(), slug, snapshot.ReasonUpload)

	err = s.store.Write(c.Request().Context(), slug, relPath, string(body), contentType, metadata)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "store asset", err)
	}

	slog.Info("upload.done", "slug", slug, "path", relPath, "type", contentType, "size", len(body), "captioned", caption.Alt != "")
	return c.JSON(http.StatusOK, uploadResponse{ //nolint:wrapcheck
		Path:        relPath,
		URL:         fmt.Sprintf("http://%s.%s:%s/%s", slug, s.domain, s.port, relPath),
		ContentType: contentType,
		Size:        len(body),
		Alt:         caption.Alt,
		Description: caption.Description,
	})
}

// captionUpload runs the vision sub-agent under a bounded deadline so a slow
// or unresponsive model can't hold the upload request open. The returned
// caption is zero-valued on failure; callers must tolerate that.
func (s *Server) captionUpload(ctx context.Context, body []byte, contentType string) (agent.Caption, error) {
	cctx, cancel := context.WithTimeout(ctx, captionTimeout)
	defer cancel()
	caption, err := agent.CaptionAsset(cctx, s.llm, body, contentType)
	if err != nil {
		return caption, fmt.Errorf("caption asset: %w", err)
	}
	return caption, nil
}

// safeAssetName produces a filesystem-safe filename derived from the
// upload's basename, forcing the extension to match the sniffed content
// type. Anything outside [a-z0-9._-] becomes a dash; empty stems become
// "asset".
func safeAssetName(original, ext string) (string, error) {
	stem := strings.TrimSuffix(path.Base(original), path.Ext(original))
	stem = strings.ToLower(stem)
	var b strings.Builder
	for _, r := range stem {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == '.':
			// collapse dots so we don't end up with a/../ shenanigans
			b.WriteRune('-')
		default:
			b.WriteRune('-')
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		cleaned = "asset"
	}
	if len(cleaned) > 60 {
		cleaned = cleaned[:60]
	}
	return cleaned + ext, nil
}
