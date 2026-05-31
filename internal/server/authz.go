package server

import (
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
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
//   - regular admin: allowed iff the slug's recorded owner matches their
//     email. Slugs with no recorded owner (pre-migration data) appear
//     not-found to regular admins — the bootstrap migration on every
//     startup keeps this from being a concern in practice.
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
	owner := s.ownerOf(slug)
	if owner == "" || owner != u.Email {
		return nil, notFound()
	}
	return u, nil
}
