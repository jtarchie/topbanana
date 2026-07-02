package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
)

// mountRoutes registers every route on the Echo instance. It is the single
// place the whole HTTP surface is declared, grouped by audience: public
// (unauthenticated), passkey auth, the MCP surface, the logged-in admin group,
// super-admin-only user management, and the per-slug site routes (each gated by
// requireSlugOwnership). New wires up middleware and the error handler, then
// hands off here.
func (s *Server) mountRoutes(e *echo.Echo) {
	// /status and /events are session-required and ownership-scoped: the
	// progress page polls them mid-build, but neither carries data a
	// non-owner should be able to subscribe to. Previously they were
	// open by design so the cookie could propagate cross-page; with the
	// passkey session cookie set on first reach to the admin surface,
	// that handoff is no longer a problem.
	e.GET("/status/:slug", s.statusHandler, s.requireUser, s.requireSlugOwnership)
	e.GET("/events/:slug", s.eventsHandler, s.requireUser, s.requireSlugOwnership)
	e.GET("/favicon.svg", s.faviconHandler)
	e.GET("/app.css", s.appCSSHandler)
	e.GET("/image_drawer.js", s.imageDrawerJSHandler)

	// Legal pages: always-public, unauthenticated. Prospective users need to
	// read these before signing up, so they can't sit behind requireUser.
	e.GET("/privacy", s.privacyHandler)
	e.GET("/terms", s.termsHandler)

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
		(&accountController{s}).registerAuthPages(e)
	}

	// MCP surface (bearer-protected /mcp + its OAuth authorization server).
	// Mounted on the main domain, outside the admin session gate — it carries
	// its own bearer-token auth and OAuth flow. Opt-in via --mcp-secret.
	if s.mcpSecret != "" {
		s.mountMCP(e)
	}

	admin := e.Group("", s.requireUser)
	owns := s.requireSlugOwnership
	admin.GET("/", s.landingHandler)
	admin.GET("/system", s.systemHandler)
	(&accountController{s}).registerAccount(admin)

	// Super-admin-only surfaces carry requireSuperAdmin (role check on top of
	// requireUser) and are mounted on the root router, not the admin group.
	(&adminController{s}).register(e, s.requireSuperAdmin)

	// Per-resource controllers. Each owns its routes and the :slug ones carry the
	// ownership gate so a regular admin gets a 404 on slugs they don't own.
	(&sitesController{s}).register(admin, owns)
	(&functionsController{s}).register(admin, owns)
	(&assetsController{s}).register(admin, owns)
	(&photowallController{s}).register(admin, owns)
	(&debugController{s}).register(admin, owns)
}
