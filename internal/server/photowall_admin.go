package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/photowall"
	"github.com/jtarchie/topbanana/internal/state"
)

// This file owns the owner-facing half of the event photo wall: the mobile-first
// moderation queue page and the approve/reject/remove actions, plus the
// owner-only preview route for un-approved bytes (which the public proxy
// refuses to serve). The visitor-facing upload/list endpoints live in
// photowall.go. Kept in-package like the rest of the admin surface — the
// handlers lean on Server's store/state/render helpers.

type photowallController struct{ *Server }

// register mounts the moderation routes under the requireUser admin group, each
// carrying the per-slug ownership gate. All are POST (not method-overridden
// DELETE) because the queue drives them with fetch() and expects JSON back.
func (s *photowallController) register(g *echo.Group, owns echo.MiddlewareFunc) {
	g.GET("/photos/:slug", s.photoQueueHandler, owns)
	g.GET("/photos/:slug/pending/:id", s.photoPendingPreviewHandler, owns)
	g.POST("/photos/:slug/approve", s.photoApproveHandler, owns)
	g.POST("/photos/:slug/reject", s.photoRejectHandler, owns)
	g.POST("/photos/:slug/remove", s.photoRemoveHandler, owns)
}

var (
	errPhotoNotFound   = errors.New("photo not found")
	errPhotoNotPending = errors.New("photo is not awaiting approval")
)

// photoQueueRow backs one card in the moderation queue.
type photoQueueRow struct {
	ID         string
	PreviewURL string // owner-only /photos/{slug}/pending/{id}
}

type photoQueueData struct {
	Chrome
	PendingCount  int
	ApprovedCount int
	Pending       []photoQueueRow
	Flash         string
}

// photoQueueHandler renders the pending photos as a stack of cards the owner
// taps through. Approved count is shown for context; the display page is the
// place approved photos actually appear.
func (s *photowallController) photoQueueHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	ctx := c.Request().Context()

	pending, approved := s.photoCounts(ctx, slug)

	rows := make([]photoQueueRow, 0, len(pending))
	for _, p := range pending {
		rows = append(rows, photoQueueRow{
			ID:         p.ID,
			PreviewURL: "/photos/" + slug + "/pending/" + p.ID,
		})
	}

	siteName := s.build.ReadMeta(ctx, slug).Title
	if siteName == "" {
		siteName = slug
	}

	return s.render(c, "photo_queue", photoQueueData{
		Chrome: Chrome{
			Slug:     slug,
			SiteName: siteName,
			SiteURL:  s.siteURL(c, slug, "/"),
			Active:   "manage",
		},
		PendingCount:  len(rows),
		ApprovedCount: approved,
		Pending:       rows,
		Flash:         c.QueryParam("flash"),
	})
}

// photoCounts loads the slug's state once and returns the pending photos
// (newest-first) and the approved count. Shared by the queue page and the
// manage page's summary card.
func (s *Server) photoCounts(ctx context.Context, slug string) (pending []photowall.Photo, approvedCount int) {
	if s.state == nil {
		return nil, 0
	}
	snap, err := s.state.Load(ctx, slug)
	if err != nil {
		slog.Warn("photowall.counts_load_failed", "slug", slug, "err", err)
		return nil, 0
	}
	pending = photowall.Collect(snap.Data, photowall.StatusPending)
	approvedCount = len(photowall.Collect(snap.Data, photowall.StatusApproved))
	return pending, approvedCount
}

// photoPendingPreviewHandler serves the bytes of one un-approved photo to the
// owner. The public proxy blocks _pending/ (proxy.go), so the moderation queue
// can't <img src> the pending bytes directly — this owner-gated route reads and
// blobs them. Only pending photos are reachable; an approved id 404s here (its
// bytes moved to the public assets/ tree).
func (s *photowallController) photoPendingPreviewHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	id := c.Param("id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing photo id")
	}
	ctx := c.Request().Context()

	if s.state == nil {
		return notFound()
	}
	snap, err := s.state.Load(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "state load", err)
	}
	p, ok := loadPhotoRow(snap.Data, id)
	if !ok || p.Status != photowall.StatusPending {
		return notFound()
	}

	obj, err := s.store.Read(ctx, slug, photowall.PendingPath(id, p.Ext))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read photo", err)
	}
	if obj.Content == "" {
		return notFound()
	}
	c.Response().Header().Set("Cache-Control", "no-store, private")
	return c.Blob(http.StatusOK, obj.ContentType, []byte(obj.Content)) //nolint:wrapcheck
}

// photoApproveHandler moves one pending photo to the public gallery. The
// confirm field must echo the id back (mirrors deleteSubmissionHandler) so a
// stray request can't approve the wrong photo.
func (s *photowallController) photoApproveHandler(c *echo.Context) error {
	return s.photoAction(c, "approve", func(ctx context.Context, slug, id string) error {
		return s.approvePhoto(ctx, slug, id)
	})
}

// photoRejectHandler discards one pending photo (bytes + row). Semantically the
// same removal as a takedown; kept as a distinct route so the queue's Reject
// button reads clearly and the two can diverge later.
func (s *photowallController) photoRejectHandler(c *echo.Context) error {
	return s.photoAction(c, "reject", func(ctx context.Context, slug, id string) error {
		return s.removePhoto(ctx, slug, id)
	})
}

// photoRemoveHandler takes an already-approved photo down (bytes + row), so an
// owner can pull something after it's gone live on the display.
func (s *photowallController) photoRemoveHandler(c *echo.Context) error {
	return s.photoAction(c, "remove", func(ctx context.Context, slug, id string) error {
		return s.removePhoto(ctx, slug, id)
	})
}

// photoAction is the shared shell for approve/reject/remove: validate the id +
// confirm echo, run the action, and answer in the format the caller wants —
// JSON for the queue's fetch() (so it can auto-advance without a reload), a
// 303 redirect for a no-JS <form> submit.
func (s *photowallController) photoAction(c *echo.Context, verb string, action func(context.Context, string, string) error) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	id := c.FormValue("id")
	if id == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "missing photo id")
	}
	if c.FormValue("confirm") != id {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match photo id")
	}

	err = action(c.Request().Context(), slug, id)
	switch {
	case errors.Is(err, errPhotoNotFound):
		return echo.NewHTTPError(http.StatusNotFound, "photo "+id+" not found")
	case errors.Is(err, errPhotoNotPending):
		return echo.NewHTTPError(http.StatusConflict, "photo "+id+" is not awaiting approval")
	case errors.Is(err, state.ErrConflict):
		return echo.NewHTTPError(http.StatusServiceUnavailable, "the wall is busy — retry shortly")
	case err != nil:
		return httpErr(http.StatusInternalServerError, verb+" photo", err)
	}

	slog.Info("photowall."+verb, "slug", slug, "id", id, "user", callerEmail(c))

	if wantsHTML(c) {
		return c.Redirect(http.StatusSeeOther, "/photos/"+slug) //nolint:wrapcheck
	}
	return c.JSON(http.StatusOK, map[string]any{"ok": true, "id": id}) //nolint:wrapcheck
}

// approvePhoto copies the pending bytes into the public assets/ tree, flips the
// metadata row to approved, then deletes the now-superseded pending bytes. CAS
// retry mirrors deleteSubmissionKey (data.go). The Copy runs before Save and is
// idempotent, so a lost CAS race just re-copies the same destination.
func (s *Server) approvePhoto(ctx context.Context, slug, id string) error {
	if s.state == nil {
		return errPhotoNotFound
	}
	key := photowall.MetaKey(id)
	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		snap, err := s.state.Load(ctx, slug)
		if err != nil {
			return fmt.Errorf("state load: %w", err)
		}
		p, ok := loadPhotoRow(snap.Data, id)
		if !ok {
			return errPhotoNotFound
		}
		if !photowall.CanTransition(p.Status, photowall.StatusApproved) {
			return errPhotoNotPending
		}

		src := photowall.PendingPath(id, p.Ext)
		dst := photowall.ApprovedPath(id, p.Ext)
		err = s.store.Copy(ctx, slug, src, dst)
		if err != nil {
			return fmt.Errorf("copy photo: %w", err)
		}

		p.Status = photowall.StatusApproved
		p.Asset = dst
		snap.Data[key] = p.ToMeta()

		err = s.state.Save(ctx, slug, snap)
		if err == nil {
			// Pending bytes are now superseded by the approved copy. Best-effort
			// delete — a leftover is proxy-blocked and harmless.
			_ = s.store.Delete(ctx, slug, src)
			return nil
		}
		if errors.Is(err, state.ErrConflict) {
			slog.Info("photowall.approve_cas_retry", "slug", slug, "id", id, "attempt", attempt+1)
			continue
		}
		return fmt.Errorf("state save: %w", err)
	}
	return state.ErrConflict
}

// removePhoto deletes a photo's bytes at both possible locations (pending and
// approved) and removes its metadata row, so it works for a reject (pending) or
// a takedown (approved) alike. Byte deletes are best-effort and idempotent;
// the row delete is CAS-guarded.
func (s *Server) removePhoto(ctx context.Context, slug, id string) error {
	if s.state == nil {
		return errPhotoNotFound
	}
	snap, err := s.state.Load(ctx, slug)
	if err != nil {
		return fmt.Errorf("state load: %w", err)
	}
	p, ok := loadPhotoRow(snap.Data, id)
	if !ok {
		return errPhotoNotFound
	}
	if p.Ext != "" {
		_ = s.store.Delete(ctx, slug, photowall.PendingPath(id, p.Ext))
		_ = s.store.Delete(ctx, slug, photowall.ApprovedPath(id, p.Ext))
	}
	return s.deletePhotoRow(ctx, slug, id)
}

// deletePhotoRow removes one photo:{id} row from the slug's KV blob under CAS
// retry. Shared by removePhoto and the upload handler's rollback path.
func (s *Server) deletePhotoRow(ctx context.Context, slug, id string) error {
	if s.state == nil {
		return errPhotoNotFound
	}
	key := photowall.MetaKey(id)
	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		snap, err := s.state.Load(ctx, slug)
		if err != nil {
			return fmt.Errorf("state load: %w", err)
		}
		if _, ok := snap.Data[key]; !ok {
			return errPhotoNotFound
		}
		delete(snap.Data, key)
		err = s.state.Save(ctx, slug, snap)
		if err == nil {
			return nil
		}
		if errors.Is(err, state.ErrConflict) {
			slog.Info("photowall.delete_cas_retry", "slug", slug, "id", id, "attempt", attempt+1)
			continue
		}
		return fmt.Errorf("state save: %w", err)
	}
	return state.ErrConflict
}

// loadPhotoRow decodes the photo:{id} row from a state snapshot's data.
func loadPhotoRow(data map[string]any, id string) (photowall.Photo, bool) {
	raw, ok := data[photowall.MetaKey(id)]
	if !ok {
		return photowall.Photo{}, false
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return photowall.Photo{}, false
	}
	return photowall.FromMeta(m)
}

// callerEmail returns the logged-in user's email, or "" if none.
func callerEmail(c *echo.Context) string {
	if u := userFromContext(c); u != nil {
		return u.Email
	}
	return ""
}
