package server

import (
	"net/http"

	"github.com/labstack/echo/v5"
	"github.com/labstack/echo/v5/middleware"

	"github.com/jtarchie/topbanana/internal/portable"
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
		e.GET("/login", s.loginHandler)
		e.GET("/register", s.registerHandler)
		e.POST("/register/finish", s.registerFinishHandler)
		e.POST("/logout", s.logoutHandler)
	}

	// MCP surface (bearer-protected /mcp + its OAuth authorization server).
	// Mounted on the main domain, outside the admin session gate — it carries
	// its own bearer-token auth and OAuth flow. Opt-in via --mcp-secret.
	if s.mcpSecret != "" {
		s.mountMCP(e)
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
	admin.POST("/account/sign-out-everywhere", s.accountSignOutEverywhereHandler)
	admin.POST("/account/delete", s.accountDeleteHandler)
	admin.POST("/account/passkeys/delete", s.accountRemovePasskeyHandler)

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
	e.POST("/admin/users/:email/delete", s.adminUserDeleteHandler, s.requireSuperAdmin)
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
	(&assetsController{s}).register(admin, owns)
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
	admin.POST("/clarify/:slug", s.clarifyHandler, owns, promptBodyCap)
}
