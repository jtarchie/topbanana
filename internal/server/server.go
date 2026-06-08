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
	"time"
	"unicode/utf8"

	"github.com/labstack/echo/v5"
	slogecho "github.com/samber/slog-echo"
	"github.com/tdewolff/minify/v2"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/assets"
	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
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
	// CustomDomainCNAME is the hostname the manage page tells users to point a
	// custom-domain CNAME record at. Empty means "use the base domain".
	CustomDomainCNAME string
}

// Deps holds the dependencies the server needs. Wired up in cmd/topbanana.
type Deps struct {
	Store    *store.Store
	Build    *build.Service
	Events   *events.Tracker
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

	// MCPSecret signs MCP bearer tokens. When non-empty it enables the MCP
	// server plus its OAuth endpoints at /mcp and /oauth/*; empty leaves the
	// whole surface unmounted. Wired from --mcp-secret.
	MCPSecret string
}

// Server is the wired-up state shared across handlers.
type Server struct {
	store      *store.Store
	build      *build.Service
	events     *events.Tracker
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

	// registry is the in-memory index of every site (custom-domain → slug,
	// slug existence, owner, privacy), rebuilt from one ListApps sweep. The
	// routing, ownership, and privacy hot paths consult it instead of S3. See
	// siteRegistry in site_registry.go.
	registry *siteRegistry

	// preWarmCert is the deps callback, captured here so settingsSubmitHandler
	// can fire it without threading the function through every signature.
	preWarmCert func(host string)

	// mcpSecret signs MCP bearer tokens; empty leaves the MCP + OAuth routes
	// unmounted. mcpOAuth holds the in-memory OAuth authorization-server state
	// (registered clients + pending authorization codes), created in New only
	// when the secret is set.
	mcpSecret string
	mcpOAuth  *mcpOAuthState
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
	// image_drawer.html defines the "image_drawer" partial used by the
	// workspace, visual-editor, and manage pages. Parse it alongside the
	// layout so any page below can reference {{ template "image_drawer" . }}.
	template.Must(tpl.Parse(imageDrawerTemplate))
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
		{"privacy", privacyTemplate},
		{"terms", termsTemplate},
	} {
		template.Must(tpl.New(t.name).Parse(t.body))
	}

	s := &Server{
		store:        d.Store,
		build:        d.Build,
		events:       d.Events,
		sandbox:      d.Sandbox,
		state:        d.State,
		snapshot:     d.Snapshot,
		auth:         d.Auth,
		domain:       d.Domain,
		port:         d.Port,
		tpl:          tpl,
		systemInfo:   d.SystemInfo,
		htmlMinifier: newHTMLMinifier(),
		registry:     newSiteRegistry(d.Store, d.Build),
		preWarmCert:  d.PreWarmCert,
		mcpSecret:    d.MCPSecret,
	}
	s.registry.initialRebuildDomainIndex(context.Background())
	if s.mcpSecret != "" {
		s.mcpOAuth = newMCPOAuthState()
	}

	e := echo.New()
	e.HTTPErrorHandler = s.httpErrorHandler
	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))
	e.Use(slogecho.New(logger))
	e.Use(hstsMiddleware())
	e.Use(s.subdomainMiddleware())

	s.mountRoutes(e)

	return e, s
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

// appCSSHandler serves the precompiled admin-UI stylesheet (Tailwind + daisyUI,
// embedded — no CDN) on the main app host. User sites carry their own /app.css
// in S3, served by proxyHandler; subdomainMiddleware routes those before they
// reach this handler.
func (s *Server) appCSSHandler(c *echo.Context) error {
	c.Response().Header().Set("Cache-Control", "public, max-age=86400")
	return c.Blob(http.StatusOK, "text/css; charset=utf-8", assets.AppCSS) //nolint:wrapcheck
}

// imageDrawerJSHandler serves the shared client module for the Images side-
// drawer. Same caching policy as the CSS sheet — the file is embedded into
// the binary, so the hash only changes on deploy.
func (s *Server) imageDrawerJSHandler(c *echo.Context) error {
	c.Response().Header().Set("Cache-Control", "public, max-age=86400")
	return c.Blob(http.StatusOK, "application/javascript; charset=utf-8", assets.ImageDrawerJS) //nolint:wrapcheck
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
func (s *sitesController) redirectToWorkspace(c *echo.Context) error {
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
func (s *sitesController) redirectToManage(c *echo.Context) error {
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
		cp := ch.chromePtr()
		cp.IsSuperAdmin = isSuper
		cp.Year = time.Now().Year()
	}
	return data
}

// landingData backs templates/landing.html. Was a map[string]any until
// the chrome refactor; the typed struct lets the shared brand partial
// pick up IsSuperAdmin via embedded promotion.
//
// Templates split into Featured (visible by default, 3 curated picks) and
// Other (behind a "More templates" disclosure). The split fights the 15-card
// cognitive wall on first paint; non-coders see a paragraph, a hero pick,
// and a button, not a wall of choices.
type landingData struct {
	Chrome
	Featured  []*templates.SiteTemplate
	Other     []*templates.SiteTemplate
	DefaultID string
	Domain    string
}

// landingFeaturedIDs are the three templates surfaced visibly on landing, in
// display order. landing-page is first AND the pre-checked default — a real
// layout sets a higher expectation for the AI output than starting from blank.
// blank stays in the curated set so power users can still pick "I'll describe
// everything." event is the third pick because party / RSVP / fundraiser is
// the most relatable use-case for the curious-non-coder primary persona.
//
// The order matters: it is the on-screen order in the .Featured slice.
// Adding a featured template = add the ID here; the rest stay in .Other.
var landingFeaturedIDs = []string{"landing-page", "blank", "event"}

// landingDefaultTemplateID is the pre-checked template on the landing form.
// Must be present in landingFeaturedIDs or the radio renders nothing checked.
// landing-page beats blank for first-timers: a real layout primes a richer
// prompt, where blank quietly invites a sparse one.
const landingDefaultTemplateID = "landing-page"

func (s *Server) landingHandler(c *echo.Context) error {
	featuredSet := make(map[string]struct{}, len(landingFeaturedIDs))
	byID := make(map[string]*templates.SiteTemplate, len(templates.All()))
	for _, id := range landingFeaturedIDs {
		featuredSet[id] = struct{}{}
	}
	for _, t := range templates.All() {
		byID[t.ID] = t
	}
	featured := make([]*templates.SiteTemplate, 0, len(landingFeaturedIDs))
	for _, id := range landingFeaturedIDs {
		if t, ok := byID[id]; ok {
			featured = append(featured, t)
		}
	}
	other := make([]*templates.SiteTemplate, 0)
	for _, t := range templates.All() {
		if _, ok := featuredSet[t.ID]; !ok {
			other = append(other, t)
		}
	}
	return s.render(c, "landing", landingData{
		Featured:  featured,
		Other:     other,
		DefaultID: landingDefaultTemplateID,
		Domain:    s.domain,
	})
}

// legalData backs templates/privacy.html and templates/terms.html. Both pages
// are unauthenticated but still embed Chrome so the brand partial and footer
// render with the current Year and (when a session is present) the super-admin
// nav link.
type legalData struct {
	Chrome
}

func (s *Server) privacyHandler(c *echo.Context) error {
	return s.render(c, "privacy", legalData{})
}

func (s *Server) termsHandler(c *echo.Context) error {
	return s.render(c, "terms", legalData{})
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
	// EditedAt is the Unix-seconds timestamp matching LastEdited, exposed so
	// the apps list can sort by recency client-side. 0 when the site has no
	// edits recorded yet (sorted last in recency view).
	EditedAt int64
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

func (s *sitesController) appsHandler(c *echo.Context) error {
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
		lastEdited, editedAt := lastEditedFor(ctx, s.Server, app)
		links = append(links, appLink{
			Name:          app,
			Title:         meta.Title,
			Description:   meta.Description,
			URL:           s.siteURL(c, app, "/"),
			LastEdited:    lastEdited,
			EditedAt:      editedAt,
			PrimaryDomain: primaryDomain,
		})
	}
	sort.SliceStable(links, func(i, j int) bool {
		return appLinkKey(links[i]) < appLinkKey(links[j])
	})

	over, quotaCap := s.appsOverQuota(user)
	return s.render(c, "apps", appsData{
		Chrome:         Chrome{Active: "apps"},
		Apps:           links,
		Flash:          c.QueryParam("flash"),
		OverQuotaCount: over,
		MaxApps:        quotaCap,
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
	quotaCap := user.Quotas.MaxApps
	if quotaCap == 0 {
		quotaCap = defaults.MaxApps
	}
	if quotaCap <= 0 {
		return 0, 0
	}
	count := s.registry.countAppsFor(user.Email)
	if count <= quotaCap {
		return 0, quotaCap
	}
	return count - quotaCap, quotaCap
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

// lastEditedFor returns a relative timestamp for the most recent transcript
// plus its Unix-seconds value, or ("", 0) when the site has no edits recorded
// yet (a freshly-created shell with no completed build, or a pre-editrec
// site). The transcript list is small (capped by retention in editrec.Trim),
// so this is O(N) per app card. The raw timestamp lets the apps template
// emit a sort key without re-parsing the humanized form.
func lastEditedFor(ctx context.Context, s *Server, slug string) (string, int64) {
	rows, err := editrec.List(ctx, s.store, slug)
	if err != nil || len(rows) == 0 {
		return "", 0
	}
	t := rows[0].Timestamp
	return humanizeAge(t), t.Unix()
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
		// still gets distinct lookup keys. Sanitizing collapses more names
		// together (e.g. "a b.md" and "a-b.md" both → "a-b.md"), so loop until
		// the candidate is genuinely unused — including against a literal name
		// a later upload might carry (e.g. someone also uploads "a-b-2.md").
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		uniq := name
		for i := 2; seen[uniq] > 0; i++ {
			uniq = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		seen[uniq]++
		out = append(out, agent.Attachment{Name: uniq, Content: string(body)})
	}
	return out, nil
}

// allowedAttachmentExts are the file extensions accepted for reference
// attachments. Markdown for prose source, HTML for existing pages the user
// wants the agent to draw from.
var allowedAttachmentExts = []string{".md", ".markdown", ".html", ".htm"}

// sanitizeAttachmentName returns a safe basename for an uploaded reference
// file. The extension must be one of allowedAttachmentExts (we can't invent
// one), but the rest of the name is sanitized rather than rejected — spaces,
// punctuation, and non-ASCII (e.g. "Caroline & Paweł.html") collapse to
// dashes so the upload just works ("caroline-pawe.html"). Always lowercase
// and limited to [a-z0-9_-] plus the extension.
func sanitizeAttachmentName(raw string) (string, error) {
	base := path.Base(strings.TrimSpace(raw))
	if base == "" || base == "." || base == "/" {
		return "", errors.New("attachment filename is empty")
	}
	lower := strings.ToLower(base)
	ext, ok := matchAttachmentExt(lower)
	if !ok {
		return "", fmt.Errorf("attachment %q must end in %s", raw, strings.Join(allowedAttachmentExts, ", "))
	}
	stem := sanitizeStem(strings.TrimSuffix(lower, ext))
	// Enforce the 80-char total cap by trimming the stem so the extension —
	// which the agent's seed loop keys off — is always preserved.
	if maxStem := 80 - len(ext); len(stem) > maxStem {
		stem = strings.Trim(stem[:maxStem], "-")
		if stem == "" {
			stem = "asset"
		}
	}
	return stem + ext, nil
}

// matchAttachmentExt reports the allowed extension a lowercased name ends in.
func matchAttachmentExt(lower string) (string, bool) {
	for _, ext := range allowedAttachmentExts {
		if strings.HasSuffix(lower, ext) {
			return ext, true
		}
	}
	return "", false
}

// sanitizeStem maps an arbitrary filename stem to a filesystem-safe slug:
// lowercased, every run of characters outside [a-z0-9_-] (spaces, dots,
// punctuation, non-ASCII) collapsed to a single dash, leading/trailing dashes
// trimmed, and an empty result replaced with "asset". Collapsing dots also
// blocks "../" path shenanigans.
func sanitizeStem(stem string) string {
	stem = strings.ToLower(stem)
	var b strings.Builder
	prevDash := false
	for _, r := range stem {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		cleaned = "asset"
	}
	return cleaned
}

func (s *sitesController) buildHandler(c *echo.Context) error {
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
		quotaErr := auth.CheckMaxApps(user, s.registry.countAppsFor(owner), s.auth.QuotaDefaults())
		if quotaErr != nil {
			return echo.NewHTTPError(http.StatusForbidden, quotaErr.Error())
		}
	}
	tiers := s.effectiveTiersFor(user)

	// templates.Get falls back to byID[defaultID]; init() panics if that's
	// missing, so tmpl is non-nil at runtime.
	slog.Info("build.start", "slug", slug, "template", tmpl.ID, "attachments", len(attachments), "owner", owner, "tiers", tiers) //nolint:nilaway // see comment.
	// Register the slug + its owner before the build kicks off so the very
	// first TLS handshake to <slug>.<domain> (the progress page about to
	// load) passes HostAllowed, and so /events + /status see ownership
	// without waiting for the next ListApps rebuild.
	s.registry.markSlug(slug)
	if owner != "" {
		s.registry.setOwner(slug, owner)
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

func (s *sitesController) editSubmitHandler(c *echo.Context) error {
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
	// EffectiveTemplate wraps templates.Get, which is non-nil at runtime
	// (init guarantees defaultID is present).
	slog.Info("edit.start", "slug", slug, "page", page, "selection_len", len(selection), "template", tmpl.ID, "seeds", len(seeds), "attachments", len(attachments), "tiers", tiers) //nolint:nilaway // see comment.
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
func (s *sitesController) relintHandler(c *echo.Context) error {
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

	// Clear deterministically fixable issues in-code before reaching for the
	// agent. AutoFix injects the /app.css substrate link (preserving all
	// existing content) and OptimizeCSS recompiles the stylesheet. A purely
	// mechanical relint — by far the common case — then re-lints clean and
	// never spawns an LLM. This matters beyond cost: the agent path has
	// regenerated pages from the error text alone and wiped site content.
	s.build.AutoFix(ctx, slug, lintErrs)
	s.build.OptimizeCSS(ctx, slug)
	residual := s.build.Lint(ctx, slug, tmpl)
	if len(residual) == 0 {
		slog.Info("relint.autofixed", "slug", slug, "fixed", len(lintErrs))
		return c.Redirect(http.StatusSeeOther, "/workspace/"+slug+"?flash=lint-autofixed") //nolint:wrapcheck
	}

	// Residual, non-mechanical issues need the agent. Relint should run
	// entirely on the Editor tier — the prompt is a lint-fix patch, well
	// within reach of the smaller model that already handles per-build
	// retries. Promoting the resolved Editor model into the Author slot of
	// the override flips every phase of the build over without having to
	// teach build.Service a per-call tier flag. Seed the agent with the
	// affected files (LintFixPrompt names them) so it edits in place rather
	// than writing blind.
	resolved := s.effectiveTiersFor(userFromContext(c))
	tiers := model.TierMap{model.TierAuthor: resolved.Resolve(model.TierEditor)}
	prompt := build.LintFixPrompt(residual)
	slog.Info("relint.start", "slug", slug, "issues", len(residual), "template", tmpl.ID, "tiers", tiers)
	return s.startBuild(c, build.Params{
		Slug:     slug,
		Prompt:   prompt,
		LogKey:   "relint",
		Template: tmpl,
		Seeds:    s.build.EditSeeds(ctx, slug, prompt),
		Tiers:    tiers,
	})
}

func (s *sitesController) settingsSubmitHandler(c *echo.Context) error {
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
	s.registry.rebuildDomainIndexLogging(ctx)
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

// DeleteAppResult reports what deleteApp removed, for the caller's audit log.
type DeleteAppResult struct {
	FilesDeleted     int
	SnapshotsDeleted int
}

// deleteApp permanently removes one app's content: every site file, every
// snapshot, and its in-memory build/event state. It deliberately does NOT
// rebuild the domain index or log — callers own that, so a single delete logs
// once and a multi-app cascade (account / user deletion) rebuilds the index
// once after the last app. Returns the count of files and snapshots removed
// for the caller's audit log. Idempotent: re-running on an already-emptied
// slug lists zero files and is a no-op, which is what makes the cascade
// retry-safe.
func (s *Server) deleteApp(ctx context.Context, slug string) (DeleteAppResult, error) {
	paths, err := s.store.List(ctx, slug)
	if err != nil {
		return DeleteAppResult{}, fmt.Errorf("list files: %w", err)
	}
	for _, p := range paths {
		derr := s.store.Delete(ctx, slug, p)
		if derr != nil {
			return DeleteAppResult{}, fmt.Errorf("delete file: %w", derr)
		}
	}

	snaps := 0
	if s.snapshot != nil {
		snapList, lerr := s.snapshot.List(ctx, slug)
		if lerr != nil {
			return DeleteAppResult{}, fmt.Errorf("list snapshots: %w", lerr)
		}
		for _, sn := range snapList {
			derr := s.snapshot.Delete(ctx, slug, sn.Key)
			if derr != nil {
				return DeleteAppResult{}, fmt.Errorf("delete snapshot: %w", derr)
			}
		}
		snaps = len(snapList)
	}

	s.events.Forget(slug)
	return DeleteAppResult{FilesDeleted: len(paths), SnapshotsDeleted: snaps}, nil
}

// deleteAppsOwnedBy cascades deleteApp over every site owned by email. It uses
// ListApps + ReadMeta as the authoritative owner source rather than the
// in-memory ownerIndex, which can lag a just-built site. It collects the first
// error but keeps going so one wedged slug doesn't strand the rest, and leaves
// the single domain-index rebuild to the caller. An empty email is a no-op —
// it must never match the empty OwnerID of a pre-migration, super-admin-only
// site. Returns the number of apps actually removed.
func (s *Server) deleteAppsOwnedBy(ctx context.Context, email string) (int, error) {
	email = auth.NormalizeEmail(email)
	if email == "" {
		return 0, nil
	}
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		meta := s.build.ReadMeta(ctx, slug)
		if meta.OwnerID != email {
			continue
		}
		res, derr := s.deleteApp(ctx, slug)
		if derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}
		slog.Info("app.delete", "slug", slug, "files", res.FilesDeleted, "snapshots", res.SnapshotsDeleted, "reason", "owner_delete")
		count++
	}
	return count, firstErr
}

// reassignAppsOwnedBy transfers every site owned by `from` to `to`, updating
// both the persisted SiteMeta.OwnerID and the in-memory owner index — the same
// per-site move transferAppHandler does, fanned across the whole owned set.
// Used when a super admin deletes a user but wants to preserve their sites
// under a new owner. Authoritative ListApps+ReadMeta scan; empty `from` is a
// no-op so it can't sweep pre-migration empty-owner sites. Collects the first
// error but keeps going, and leaves the single index rebuild to the caller.
// Returns the number of apps reassigned.
func (s *Server) reassignAppsOwnedBy(ctx context.Context, from, to string) (int, error) {
	from = auth.NormalizeEmail(from)
	to = auth.NormalizeEmail(to)
	if from == "" {
		return 0, nil
	}
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		meta := s.build.ReadMeta(ctx, slug)
		if meta.OwnerID != from {
			continue
		}
		meta.OwnerID = to
		werr := s.build.WriteMeta(ctx, slug, meta)
		if werr != nil {
			if firstErr == nil {
				firstErr = werr
			}
			continue
		}
		s.registry.setOwner(slug, to)
		slog.Info("app.transfer", "slug", slug, "from", from, "to", to, "reason", "owner_delete")
		count++
	}
	return count, firstErr
}

// settingsDeleteHandler permanently removes an app: all site files, all
// snapshots, the in-memory build status, and any custom-domain mapping. The
// caller must POST `confirm` equal to the slug — the typed-slug guard is the
// only safety check.
func (s *sitesController) settingsDeleteHandler(c *echo.Context) error {
	slug := c.Param("slug")
	err := validateSlug(slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if c.FormValue("confirm") != slug {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match slug")
	}

	ctx := c.Request().Context()
	res, err := s.deleteApp(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete app", err)
	}
	s.registry.rebuildDomainIndexLogging(ctx)

	slog.Info("app.delete", "slug", slug, "files", res.FilesDeleted, "snapshots", res.SnapshotsDeleted)
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
		if other, ok := s.registry.lookupCustomDomain(host); ok && other != owningSlug {
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

// storeUploadedAsset is the shared core behind the web /upload handler and the
// MCP upload-ticket handler (mcp_uploads.go). Given the already-read bytes and
// the client's suggested filename, it sniffs + allowlists the content type
// (the extension is forced from the sniff, never trusted), derives a safe
// assets/ path, best-effort captions, snapshots, and writes. Returns an
// echo.HTTPError carrying the right 4xx/5xx status for both callers to surface.
func (s *Server) storeUploadedAsset(ctx context.Context, slug, filename string, body []byte) (uploadResponse, error) {
	contentType := http.DetectContentType(body)
	contentType = strings.SplitN(contentType, ";", 2)[0]
	ext, ok := allowedAssetTypes[contentType]
	if !ok {
		// SVG sniffs as text/xml or text/plain; trust the extension when the
		// upload looks textual.
		if e := strings.ToLower(path.Ext(filename)); e == ".svg" {
			contentType = "image/svg+xml"
			ext = ".svg"
			ok = true
		}
	}
	if !ok {
		return uploadResponse{}, echo.NewHTTPError(http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported type %q (allowed: jpeg, png, gif, webp, svg)", contentType))
	}

	name, err := safeAssetName(filename, ext)
	if err != nil {
		return uploadResponse{}, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	relPath := uploadAssetsDir + "/" + name

	// Caption synchronously so the UI can show the suggested alt-text right
	// next to the upload, and so the agent's first list_assets call already
	// has the metadata. Failures here are non-fatal — the upload still lands.
	caption, captionErr := s.captionUpload(ctx, body, contentType)
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

	s.snapshotBefore(ctx, slug, snapshot.ReasonUpload)

	err = s.store.Write(ctx, slug, relPath, string(body), contentType, metadata)
	if err != nil {
		return uploadResponse{}, httpErr(http.StatusInternalServerError, "store asset", err)
	}

	slog.Info("upload.done", "slug", slug, "path", relPath, "type", contentType, "size", len(body), "captioned", caption.Alt != "")
	return uploadResponse{
		Path:        relPath,
		URL:         fmt.Sprintf("http://%s.%s:%s/%s", slug, s.domain, s.port, relPath),
		ContentType: contentType,
		Size:        len(body),
		Alt:         caption.Alt,
		Description: caption.Description,
	}, nil
}

// captionUpload runs the vision sub-agent under a bounded deadline so a slow
// or unresponsive model can't hold the upload request open. The returned
// caption is zero-valued on failure; callers must tolerate that.
func (s *Server) captionUpload(ctx context.Context, body []byte, contentType string) (agent.Caption, error) {
	cctx, cancel := context.WithTimeout(ctx, captionTimeout)
	defer cancel()
	// Resolve the vision-tier model through the build service so captioning
	// shares the same per-model client cache and tier-fallback rules as the
	// rest of the app: an empty LLM_VISION_MODEL falls back to the author
	// model (LLM_MODEL), exactly like the editor/utility tiers. A missing
	// factory (tests) or unresolvable tier yields an error, which the caller
	// logs as a warning — the upload still succeeds without alt-text.
	llm, _, err := s.build.LLMForTier(cctx, model.TierMap{}, model.TierVision)
	if err != nil {
		return agent.Caption{}, fmt.Errorf("resolve vision model: %w", err)
	}
	caption, err := agent.CaptionAsset(cctx, llm, body, contentType)
	if err != nil {
		return caption, fmt.Errorf("caption asset: %w", err)
	}
	return caption, nil
}

// safeAssetName produces a filesystem-safe filename derived from the
// upload's basename, forcing the extension to match the sniffed content
// type. Shares sanitizeStem with attachment names: anything outside
// [a-z0-9_-] collapses to a dash and empty stems become "asset".
func safeAssetName(original, ext string) (string, error) {
	stem := sanitizeStem(strings.TrimSuffix(path.Base(original), path.Ext(original)))
	if len(stem) > 60 {
		stem = strings.Trim(stem[:60], "-")
		if stem == "" {
			stem = "asset"
		}
	}
	return stem + ext, nil
}
