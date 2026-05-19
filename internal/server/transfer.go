package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/buildabear/internal/auth"
)

// transferAppHandler moves ownership of an app to another user. Gated by
// requireSlugOwnership upstream, so the caller is either the current
// owner or a super admin. Allows transferring to a recipient who would
// land over their MaxApps cap — they just get a "you're over quota"
// banner on /apps until they delete one. Rejects:
//
//   - transfer-to-self  (no-op, almost certainly a typo)
//   - disabled recipient (can't sign in to do anything with it)
//   - missing recipient  (email not found)
func (s *Server) transferAppHandler(c *echo.Context) error {
	slug := c.Param("slug")
	target := auth.NormalizeEmail(c.FormValue("new_owner_email"))
	if target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "new_owner_email required")
	}

	ctx := c.Request().Context()
	caller := userFromContext(c)
	if caller != nil && target == caller.Email {
		return echo.NewHTTPError(http.StatusBadRequest, "cannot transfer to yourself")
	}

	recipient, err := s.auth.Users.Load(ctx, target)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "no user with that email")
		}
		return httpErr(http.StatusInternalServerError, "load recipient", err)
	}
	if recipient.Disabled {
		return echo.NewHTTPError(http.StatusBadRequest, "recipient account is disabled")
	}

	meta := s.build.ReadMeta(ctx, slug)
	previousOwner := meta.OwnerID
	meta.OwnerID = recipient.Email
	err = s.build.WriteMeta(ctx, slug, meta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "write meta", err)
	}
	s.setOwner(slug, recipient.Email)

	callerEmail := ""
	if caller != nil {
		callerEmail = caller.Email
	}
	slog.Info("app.transfer", "slug", slug, "from", previousOwner, "to", recipient.Email, "by", callerEmail)

	// Bounce the caller to /apps with a flash. Regular admins lose access
	// the moment the redirect lands, since their slug is no longer in
	// their owned set; super admins keep seeing it (with the new owner).
	flash := fmt.Sprintf("transferred+%s+to+%s", slug, recipient.Email)
	return c.Redirect(http.StatusSeeOther, "/apps?flash="+flash) //nolint:wrapcheck
}
