package server

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/portable"
)

// exportHandler streams a tar.zst archive of the slug's site files back to
// the caller as an attachment. requireSlugOwnership gates the route so only
// the owner (or a super admin) can download. No snapshot is taken — export
// is read-only.
func (s *Server) exportHandler(c *echo.Context) error {
	slug := c.Param("slug")
	ctx := c.Request().Context()

	meta := s.build.ReadMeta(ctx, slug)

	archive, err := portable.Export(ctx, s.store, slug, meta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "build export archive", err)
	}

	filename := fmt.Sprintf("%s-%s%s", slug, time.Now().UTC().Format("20060102T150405Z"), portable.ArchiveExt)
	c.Response().Header().Set("Content-Type", portable.ArchiveContentType)
	c.Response().Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Response().Header().Set("Content-Length", strconv.Itoa(len(archive)))

	slog.Info("app.export", "slug", slug, "bytes", len(archive))

	_, err = c.Response().Write(archive)
	if err != nil {
		// Client likely disconnected mid-download; logging suffices.
		slog.Warn("export.write_failed", "slug", slug, "err", err)
	}
	return nil
}

// importHandler accepts a tar.zst archive and materialises it as a new site
// under a new slug owned by the caller. The slug is only registered in the
// in-memory index after extraction succeeds, so a partial import never shows
// up in /apps or in subdomain routing — failed imports tidy themselves and
// leave no trace.
func (s *Server) importHandler(c *echo.Context) error {
	ctx := c.Request().Context()

	caller := userFromContext(c)
	if caller == nil {
		// requireUser gates the group, but the nil guard keeps a future
		// refactor from silently dropping the auth context.
		return notFound()
	}

	if s.auth != nil {
		quotaErr := auth.CheckMaxApps(caller, s.countAppsFor(caller.Email), s.auth.QuotaDefaults())
		if quotaErr != nil {
			if errors.Is(quotaErr, auth.ErrMaxAppsReached) {
				return echo.NewHTTPError(http.StatusForbidden, quotaErr.Error())
			}
			return httpErr(http.StatusInternalServerError, "check quota", quotaErr)
		}
	}

	header, err := c.FormFile("archive")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "archive file is required")
	}
	if header.Size > portable.MaxArchiveBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("archive exceeds %d bytes", portable.MaxArchiveBytes))
	}

	src, err := header.Open()
	if err != nil {
		return httpErr(http.StatusInternalServerError, "open upload", err)
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, portable.MaxArchiveBytes+1))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if len(body) > portable.MaxArchiveBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("archive exceeds %d bytes", portable.MaxArchiveBytes))
	}

	requested := strings.TrimSpace(c.FormValue("slug"))
	slug, err := s.resolveSlug(ctx, requested)
	if err != nil {
		return err
	}

	result, err := portable.Import(ctx, s.store, slug, body)
	if err != nil {
		// Tidy any partial state under the slug so a retry can re-use it
		// and so the bucket doesn't keep half-imported leftovers around.
		cleanupErr := portable.Cleanup(ctx, s.store, slug)
		if cleanupErr != nil {
			slog.Warn("import.cleanup_failed", "slug", slug, "err", cleanupErr)
		}
		return mapImportError(err)
	}

	meta := build.SiteMeta{
		Template:    result.Template,
		Title:       result.Title,
		Description: result.Description,
		Created:     time.Now().UTC(),
		OwnerID:     caller.Email,
	}
	err = s.build.WriteMeta(ctx, slug, meta)
	if err != nil {
		// Roll back the extracted files so the slug is reclaimable.
		cleanupErr := portable.Cleanup(ctx, s.store, slug)
		if cleanupErr != nil {
			slog.Warn("import.cleanup_failed", "slug", slug, "err", cleanupErr)
		}
		return httpErr(http.StatusInternalServerError, "write imported meta", err)
	}

	s.markSlug(slug)
	s.setOwner(slug, caller.Email)
	s.snapshotBefore(ctx, slug, "import")
	s.rebuildDomainIndexLogging(ctx)

	slog.Info("app.import", "slug", slug, "files", result.FileCount, "by", caller.Email)
	return c.Redirect(http.StatusSeeOther, "/workspace/"+slug) //nolint:wrapcheck
}

// mapImportError translates package-level sentinel errors into HTTP responses
// the user can act on. Anything unknown surfaces as a 500 so the caller still
// sees a structured error rather than a raw panic.
func mapImportError(err error) error {
	switch {
	case errors.Is(err, portable.ErrArchiveTooLarge):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, err.Error())
	case errors.Is(err, portable.ErrNoIndex):
		return echo.NewHTTPError(http.StatusBadRequest, "archive is missing index.html — every site needs a homepage")
	case errors.Is(err, portable.ErrCorrupt):
		return echo.NewHTTPError(http.StatusBadRequest, "archive is not a valid .tar.zst file")
	case errors.Is(err, portable.ErrTooManyFiles):
		return echo.NewHTTPError(http.StatusBadRequest, fmt.Sprintf("archive has too many files (max %d)", portable.MaxFileCount))
	case errors.Is(err, portable.ErrExtractedTooBig):
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("archive contents exceed %d bytes uncompressed", portable.MaxExtractedBytes))
	default:
		return httpErr(http.StatusInternalServerError, "import archive", err)
	}
}
