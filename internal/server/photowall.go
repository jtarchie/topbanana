package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/photowall"
	"github.com/jtarchie/topbanana/internal/state"
)

// This file owns the visitor-facing (site-host) half of the event photo wall:
// the unauthenticated POST /_photos upload endpoint and the GET
// /_photos/approved polling endpoint the full-screen display reads. Both are
// dispatched from dispatchSite (dispatch.go), NOT the /api/* functions runtime
// — that runtime caps bodies at 256 KB and never parses multipart, so binary
// photo intake needs its own Go path. The owner-facing moderation half lives in
// photowall_admin.go.

// photoWallEnabled reports whether the reserved photo endpoints are live for a
// slug: the site's resolved template opts into the wall. Mirrors apiHandler's
// EnablesFunctions gate (api.go) so no other site exposes an open upload URL.
func (s *Server) photoWallEnabled(ctx context.Context, slug string) bool {
	meta := s.build.ReadMeta(ctx, slug)
	tmpl := build.EffectiveTemplate(meta)
	return tmpl != nil && tmpl.EnablesPhotoWall
}

// dispatchPhotoWall handles the two reserved site-host photo paths. It returns
// handled=false for any other path so dispatchSite falls through to /api or the
// static proxy. When the path shape matches but the feature is off (or the
// method is wrong), it claims the request and 404s — the endpoints are
// invisible on every non-photo-wall site.
func (s *Server) dispatchPhotoWall(c *echo.Context, slug, reqPath string) (bool, error) {
	if reqPath != "/_photos" && reqPath != "/_photos/approved" && reqPath != "/_photos/qr" {
		return false, nil
	}
	if !s.photoWallEnabled(c.Request().Context(), slug) {
		return true, notFound()
	}
	switch {
	case reqPath == "/_photos" && c.Request().Method == http.MethodPost:
		return true, s.photoUploadHandler(c, slug)
	case reqPath == "/_photos/approved" && c.Request().Method == http.MethodGet:
		return true, s.approvedListHandler(c, slug)
	case reqPath == "/_photos/qr" && c.Request().Method == http.MethodGet:
		return true, s.photoQRHandler(c)
	default:
		return true, notFound()
	}
}

// photoQRHandler serves an SVG QR code that encodes this site's own upload page
// (the request host's root). The display page shows it in a corner so guests
// can scan the big screen and open the upload page. Built from the request host
// so it points at whatever domain the display is being viewed on (subdomain or
// custom domain). Cacheable — the encoded URL is stable per host.
func (s *Server) photoQRHandler(c *echo.Context) error {
	uploadURL := c.Scheme() + "://" + c.Request().Host + "/"
	svg, err := photowall.QRCodeSVG(uploadURL)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "render qr", err)
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=3600")
	return c.Blob(http.StatusOK, "image/svg+xml; charset=utf-8", []byte(svg)) //nolint:wrapcheck
}

// photoUploadHandler accepts one multipart photo from an unauthenticated
// visitor (the QR-code flow). It rate-limits per (slug, IP), sniffs and
// allowlists the image type, enforces the outstanding-pending cap, then writes
// the bytes to the reserved _pending/ prefix and records a pending metadata
// row. Every photo lands pending — nothing is shown until the owner approves.
func (s *Server) photoUploadHandler(c *echo.Context, slug string) error {
	ctx := c.Request().Context()

	if !s.photoLimiter.Allow(slug + "|" + c.RealIP()) {
		return echo.NewHTTPError(http.StatusTooManyRequests, "too many uploads — please wait a moment and try again")
	}

	body, contentType, ext, err := readUploadedPhoto(c)
	if err != nil {
		return err
	}

	if s.state == nil {
		return httpErr(http.StatusInternalServerError, "store photo", errors.New("state backend not configured"))
	}

	// CAS loop: reserve the next id, record the pending row, then write the
	// bytes once the row commits (so a lost CAS race never orphans bytes in
	// _pending/). Mirrors deleteSubmissionKey's retry shape (data.go).
	for attempt := 0; attempt <= maxCASRetries; attempt++ {
		snap, err := s.state.Load(ctx, slug)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "state load", err)
		}
		if snap.Data == nil {
			snap.Data = map[string]any{}
		}
		if photowall.CountPending(snap.Data) >= photowall.DefaultPendingCap {
			return echo.NewHTTPError(http.StatusTooManyRequests, "the wall is full right now — the host will approve photos to make room")
		}

		seq := nextPhotoSeq(snap.Data)
		id := photowall.FormatID(seq)
		photo := photowall.Photo{ID: id, Status: photowall.StatusPending, Ext: ext, TS: time.Now().UnixMilli()}
		snap.Data[photowall.SeqKey] = seq
		snap.Data[photowall.MetaKey(id)] = photo.ToMeta()

		err = s.state.Save(ctx, slug, snap)
		if err == nil {
			werr := s.store.Write(ctx, slug, photowall.PendingPath(id, ext), string(body), contentType, nil)
			if werr != nil {
				// Roll back the row so the queue never shows a photo with no
				// bytes. Best-effort — a failed rollback just leaves a preview
				// that 404s, which the queue tolerates.
				_ = s.deletePhotoRow(ctx, slug, id)
				return httpErr(http.StatusInternalServerError, "store photo", werr)
			}
			slog.Info("photowall.upload", "slug", slug, "id", id, "type", contentType, "size", len(body))
			// See Other back to the upload page with a success banner: a plain
			// <form> submit lands on a friendly page, and a fetch() client can
			// ignore the redirect body and show its own confirmation.
			return c.Redirect(http.StatusSeeOther, "/?submitted=1") //nolint:wrapcheck
		}
		if errors.Is(err, state.ErrConflict) {
			slog.Info("photowall.upload_cas_retry", "slug", slug, "attempt", attempt+1)
			continue
		}
		return httpErr(http.StatusInternalServerError, "state save", err)
	}
	return echo.NewHTTPError(http.StatusServiceUnavailable, "the wall is busy — please try again in a moment")
}

// readUploadedPhoto pulls the multipart "photo" file, enforces the size cap
// with the same read loop as the admin upload (assets.go — trust neither the
// Size header nor the client), then sniffs and allowlists the image type. The
// extension is forced from the sniff (uploads.go idiom) so a renamed file can't
// smuggle past; SVG is excluded, unlike the admin asset allowlist, since a
// photo wall takes photographs. Returns an echo.HTTPError ready to surface.
func readUploadedPhoto(c *echo.Context) (body []byte, contentType, ext string, err error) {
	header, err := c.FormFile("photo")
	if err != nil {
		return nil, "", "", echo.NewHTTPError(http.StatusBadRequest, "a photo file is required")
	}
	if header.Size > maxUploadBytes {
		return nil, "", "", echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("photo exceeds %d bytes", maxUploadBytes))
	}
	src, err := header.Open()
	if err != nil {
		return nil, "", "", httpErr(http.StatusInternalServerError, "open upload", err)
	}
	defer func() { _ = src.Close() }()
	body, err = io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		return nil, "", "", httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if len(body) > maxUploadBytes {
		return nil, "", "", echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("photo exceeds %d bytes", maxUploadBytes))
	}

	contentType = strings.SplitN(http.DetectContentType(body), ";", 2)[0]
	ext, ok := photowall.ExtForContentType(contentType)
	if !ok {
		return nil, "", "", echo.NewHTTPError(http.StatusUnsupportedMediaType, "unsupported file — upload a JPEG, PNG, GIF, or WebP photo")
	}
	return body, contentType, ext, nil
}

// approvedListHandler returns the approved photos as JSON for the display page
// to poll: [{url, ts}], newest-first. url is a site-relative path so it
// resolves on whichever host the display loaded from (subdomain or custom
// domain). no-store so a newly-approved photo shows up on the next poll.
func (s *Server) approvedListHandler(c *echo.Context, slug string) error {
	setAPICacheHeaders(c)

	type item struct {
		URL string `json:"url"`
		TS  int64  `json:"ts"`
	}
	out := []item{}

	if s.state != nil {
		snap, err := s.state.Load(c.Request().Context(), slug)
		if err != nil {
			return httpErr(http.StatusInternalServerError, "state load", err)
		}
		for _, p := range photowall.Collect(snap.Data, photowall.StatusApproved) {
			if p.Asset == "" {
				continue
			}
			out = append(out, item{URL: "/" + p.Asset, TS: p.TS})
		}
	}
	return c.JSON(http.StatusOK, out) //nolint:wrapcheck
}

// nextPhotoSeq returns the id sequence value to assign to the next photo,
// coercing the numeric forms photo_seq can take after a JSON round-trip
// (float64) or a direct Go write (int64/int).
func nextPhotoSeq(data map[string]any) int64 {
	switch n := data[photowall.SeqKey].(type) {
	case float64:
		return int64(n) + 1
	case int64:
		return n + 1
	case int:
		return int64(n) + 1
	default:
		return 1
	}
}
