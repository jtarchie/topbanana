package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/auth"
)

// requireSlugOwnership is the Echo middleware applied to every per-slug
// admin route. It pulls `:slug` from the route, runs the ownership
// check via authorizeSlug, and rejects with 404 if the caller doesn't
// own the app and isn't a super admin. Stashed on the per-route slot
// rather than baked into requireUser because the param isn't available
// at the group level.
func (s *Server) requireSlugOwnership(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		slug := c.Param("slug")
		if slug == "" {
			return next(c)
		}
		validateErr := validateSlug(slug)
		if validateErr != nil {
			return echo.NewHTTPError(http.StatusBadRequest, validateErr.Error())
		}
		_, authzErr := s.authorizeSlug(c, slug)
		if authzErr != nil {
			return authzErr
		}
		return next(c)
	}
}

// authorizeSlug is the single ownership gate every :slug handler calls
// at entry. Returns the logged-in user (set by requireUser earlier in
// the middleware chain) when the access is allowed; returns a 404
// (deliberately not 403) so a regular admin probing slugs they don't own
// can't tell the difference between "doesn't exist" and "exists but not
// yours".
//
// Authorization rules:
//   - super admin: always allowed.
//   - regular admin: allowed iff they own the slug or appear in its
//     collaborator list (registry.canAccess). Slugs with no recorded owner
//     (pre-migration data) appear not-found to regular admins — the bootstrap
//     migration on every startup keeps this from being a concern in practice.
func (s *Server) authorizeSlug(c *echo.Context, slug string) (*auth.User, error) {
	u := userFromContext(c)
	if u == nil {
		// Shouldn't reach here if the gate ran, but guard anyway —
		// failing closed is the safe default.
		return nil, notFound()
	}
	if u.Role == auth.RoleSuperAdmin {
		return u, nil
	}
	if !s.registry.canAccess(slug, u.Email) {
		return nil, notFound()
	}
	return u, nil
}

// requireSlugOwner is the stricter gate layered on the handful of routes a
// collaborator must not reach: deleting the site, transferring it, and editing
// the collaborator list itself. It runs after requireSlugOwnership, so the
// caller has already been proven to have access — which is why the rejection
// is a plain 403 rather than the 404 authorizeSlug uses. There is nothing left
// to leak at this point, and a 404 here would read as a broken link on a page
// the user is looking at.
func (s *Server) requireSlugOwner(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		slug := c.Param("slug")
		u := userFromContext(c)
		if u == nil {
			return notFound()
		}
		if s.isOwner(slug, u) {
			return next(c)
		}
		return echo.NewHTTPError(http.StatusForbidden, "only the site owner can do that")
	}
}

// isOwner is the counterpart to registry.canAccess: one definition of "may do
// the owner-only things", used by both the gate and the manage page that
// decides whether to render them. Two surfaces deriving it separately is how a
// page ends up offering a button the gate refuses (or hiding one it allows).
// Empty email never matches, for the same reason canAccess refuses it: it must
// not collide with the empty OwnerID of a pre-migration site.
func (s *Server) isOwner(slug string, u *auth.User) bool {
	if u == nil {
		return false
	}
	if u.Role == auth.RoleSuperAdmin {
		return true
	}
	email := auth.NormalizeEmail(u.Email)
	if email == "" {
		return false
	}
	return s.registry.ownerOf(slug) == email
}
