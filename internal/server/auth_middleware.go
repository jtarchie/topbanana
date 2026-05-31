package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
)

// userContextKey is the Echo context slot that RequireUser populates.
// Handlers fetch the logged-in user via userFromContext below.
const userContextKey = "auth.user"

// stripPort drops the trailing :port off a Host header, leaving just the
// bare hostname. Used in every host-based check (admin gate, autocert
// HostPolicy, custom-domain lookup) so callers see one canonical form.
func stripPort(host string) string {
	for i := len(host) - 1; i >= 0; i-- {
		if host[i] == ':' {
			return host[:i]
		}
		if host[i] == ']' {
			return host
		}
	}
	return host
}

// isMainDomainHost reports whether host is the main app domain, a loopback
// alias, or a subdomain of the main app domain. Anything else is a custom
// domain (or unknown).
func (s *Server) isMainDomainHost(host string) bool {
	if host == s.domain || fallThroughHosts[host] {
		return true
	}
	if len(host) > len(s.domain)+1 && host[len(host)-len(s.domain)-1:] == "."+s.domain {
		return true
	}
	return false
}

// requireUser is the post-cutover replacement for requireAdmin. The host
// check stays the same so admin routes are still only reachable via the
// main domain or one of the loopback aliases. After that, instead of
// checking HTTP Basic Auth + a signed cookie, we look up the passkey
// library's session cookie, resolve it to a user record, and stash the
// user in the request context for handlers.
//
// On any failure the request is redirected to /login — a 401 / 404 would
// leave the user staring at a blank page without a way back in.
func (s *Server) requireUser(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		host := stripPort(c.Request().Host)
		if !s.isMainDomainHost(host) {
			return notFound()
		}
		if s.auth == nil {
			// Should never happen post-cutover — main.go requires
			// SUPER_ADMIN_EMAIL — but failing closed beats panicking.
			return notFound()
		}
		email, ok := s.currentSessionEmail(c)
		if !ok {
			return c.Redirect(http.StatusSeeOther, "/login")
		}
		user, err := s.auth.Users.LookupCached(c.Request().Context(), email)
		if err != nil {
			s.auth.Passkey.Logout(c.Response(), c.Request())
			return c.Redirect(http.StatusSeeOther, "/login")
		}
		if user.Disabled {
			s.auth.Passkey.Logout(c.Response(), c.Request())
			return c.Redirect(http.StatusSeeOther, "/login")
		}
		c.Set(userContextKey, user)
		return next(c)
	}
}

// userFromContext returns the user set by requireUser. Used by handlers
// downstream of the gate to read role + email for ownership checks
// (wired in commit 5).
//

func userFromContext(c *echo.Context) *auth.User {
	u, _ := c.Get(userContextKey).(*auth.User)
	return u
}

// requireSuperAdmin chains requireUser and then rejects with a 404 if
// the logged-in user isn't a super admin. 404 rather than 403 so the
// existence of these routes doesn't leak to a regular admin probing
// /admin/users.
func (s *Server) requireSuperAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	gated := s.requireUser(func(c *echo.Context) error {
		u := userFromContext(c)
		if u == nil || u.Role != auth.RoleSuperAdmin {
			return notFound()
		}
		return next(c)
	})
	return gated
}

// canEdit reports whether the session belongs to a user who can edit the
// given slug — either the recorded owner or any super admin. Used by
// injectEditToolbar to keep the floating toolbar off pages a regular
// admin happens to be viewing on someone else's site.
func (s *Server) canEdit(c *echo.Context, slug string) bool {
	if s.auth == nil {
		return false
	}
	email, ok := s.currentSessionEmail(c)
	if !ok {
		return false
	}
	user, err := s.auth.Users.LookupCached(c.Request().Context(), email)
	if err != nil {
		return false
	}
	if user.Disabled {
		return false
	}
	if user.Role == auth.RoleSuperAdmin {
		return true
	}
	return s.ownerOf(slug) == user.Email
}
