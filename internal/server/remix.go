package server

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
)

// remixHandler duplicates an existing app into a fresh slug owned by the
// caller. Gated by requireSlugOwnership upstream, so the source must
// already belong to the caller (or they're a super admin). The copy
// is independent — later edits on the source do not bleed into the
// duplicate. Custom domains and the Private flag are intentionally not
// carried over; the new site starts clean and visible.
func (s *Server) remixHandler(c *echo.Context) error {
	srcSlug := c.Param("slug")
	ctx := c.Request().Context()

	caller := userFromContext(c)
	if caller == nil {
		// requireUser already gates this group; the nil check is a
		// belt-and-suspenders guard so a future refactor can't silently
		// drop the auth context.
		return notFound()
	}

	srcMeta := s.build.ReadMeta(ctx, srcSlug)

	if s.auth != nil {
		quotaErr := auth.CheckMaxApps(caller, s.countAppsFor(caller.Email), s.auth.QuotaDefaults())
		if quotaErr != nil {
			if errors.Is(quotaErr, auth.ErrMaxAppsReached) {
				return echo.NewHTTPError(http.StatusForbidden, quotaErr.Error())
			}
			return httpErr(http.StatusInternalServerError, "check quota", quotaErr)
		}
	}

	dstSlug, err := s.allocateSlug(ctx)
	if err != nil {
		return err
	}

	paths, err := s.store.List(ctx, srcSlug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list source files", err)
	}
	for _, p := range paths {
		// The meta sidecar gets rewritten below with the new owner; the
		// legacy name is kept reserved so we never resurrect a stale one
		// on a duplicate.
		if p == build.MetaFile || p == ".buildabear.json" {
			continue
		}
		obj, readErr := s.store.Read(ctx, srcSlug, p)
		if readErr != nil {
			return httpErr(http.StatusInternalServerError, "read source file", readErr)
		}
		if obj.Content == "" {
			continue
		}
		writeErr := s.store.Write(ctx, dstSlug, p, obj.Content, obj.ContentType, obj.Metadata)
		if writeErr != nil {
			return httpErr(http.StatusInternalServerError, "write copy", writeErr)
		}
	}

	dstMeta := build.SiteMeta{
		Template:         srcMeta.Template,
		Created:          time.Now().UTC(),
		Title:            srcMeta.Title,
		Description:      srcMeta.Description,
		EnablesFunctions: srcMeta.EnablesFunctions,
		EnablesPublicAPI: srcMeta.EnablesPublicAPI,
		OwnerID:          caller.Email,
		// Domains and Private intentionally not carried — a remix is a
		// new site that has to claim its own hostnames and visibility.
	}
	err = s.build.WriteMeta(ctx, dstSlug, dstMeta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "write remix meta", err)
	}

	s.markSlug(dstSlug)
	s.setOwner(dstSlug, caller.Email)
	s.rebuildDomainIndexLogging(ctx)

	slog.Info("app.remix", "from", srcSlug, "to", dstSlug, "by", caller.Email)
	flash := fmt.Sprintf("remixed+%s+as+%s", srcSlug, dstSlug)
	return c.Redirect(http.StatusSeeOther, "/manage/"+dstSlug+"?flash="+flash) //nolint:wrapcheck
}
