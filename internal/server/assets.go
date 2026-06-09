package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// assetsController serves the per-site image library: upload, list, edit
// metadata, delete. It embeds *Server for the shared deps and helpers (store,
// siteURL, snapshotBefore, storeUploadedAsset) every handler here leans on.
type assetsController struct{ *Server }

// register mounts the asset routes on the owner-scoped admin group.
func (s *assetsController) register(g *echo.Group, owns echo.MiddlewareFunc) {
	g.POST("/upload/:slug", s.uploadHandler, owns)
	g.GET("/assets/:slug", s.assetsListHandler, owns)
	g.PATCH("/assets/:slug/*", s.assetMetadataPatchHandler, owns)
	g.DELETE("/assets/:slug/*", s.assetDeleteHandler, owns)
}

// assetMaxAltLen mirrors the cap the vision-captioner enforces (alt-text is
// supposed to fit screen-reader patience; the value is reused for user edits
// so the limit is consistent regardless of who wrote the alt).
const assetMaxAltLen = 125

// assetMaxDescriptionLen is a generous cap so a description still reads as a
// sentence rather than a paragraph. The form input is a single-line textarea
// in the drawer; this keeps the column on the agent's list_assets tool
// response from ballooning either.
const assetMaxDescriptionLen = 500

// assetEntry is one row of the JSON returned by assetsListHandler. URL points
// at the live subdomain so the drawer can render a thumbnail directly.
type assetEntry struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	Alt         string `json:"alt"`
	Description string `json:"description"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Modified    string `json:"modified,omitempty"`
}

// assetsListHandler returns every object under `{slug}/assets/` with the
// alt/description metadata the drawer renders alongside thumbnails. Mirrors
// the agent's list_assets tool but adds size/modified/URL the UI needs.
func (s *assetsController) assetsListHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}

	entries, err := s.store.ListWithMeta(c.Request().Context(), slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list assets", err)
	}

	out := make([]assetEntry, 0)
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, uploadAssetsDir+"/") {
			continue
		}
		row := assetEntry{
			Path: e.Path,
			URL:  s.siteURL(c, slug, "/"+e.Path),
			Size: e.Size,
		}
		if !e.LastModified.IsZero() {
			row.Modified = e.LastModified.UTC().Format(time.RFC3339)
		}
		// Pull metadata via Read so the drawer shows the same alt/description
		// the agent sees. The store caches reads so a follow-up list_assets
		// call within the request lifecycle stays cheap.
		obj, readErr := s.store.Read(c.Request().Context(), slug, e.Path)
		if readErr == nil && obj != nil {
			row.Alt = obj.Metadata["alt"]
			row.Description = obj.Metadata["description"]
			row.ContentType = obj.ContentType
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return c.JSON(http.StatusOK, out) //nolint:wrapcheck
}

// assetMetadataPatch is the JSON body the drawer's detail view POSTs when the
// user saves an edit. Both fields are required (use empty strings to clear).
type assetMetadataPatch struct {
	Alt         string `json:"alt"`
	Description string `json:"description"`
}

// normalizeAssetPath validates a wildcard-route asset path: prepends the
// `assets/` prefix if missing, rejects traversal segments. Returns the
// normalized path or a 4xx-shaped echo error suitable for direct return.
func normalizeAssetPath(raw string) (string, error) {
	if raw == "" {
		return "", echo.NewHTTPError(http.StatusBadRequest, "asset path is required")
	}
	if !strings.HasPrefix(raw, uploadAssetsDir+"/") {
		raw = uploadAssetsDir + "/" + raw
	}
	if strings.Contains(raw, "..") || strings.HasPrefix(raw, "/") {
		return "", echo.NewHTTPError(http.StatusBadRequest, "invalid asset path")
	}
	return raw, nil
}

// decodeAssetPatch reads the JSON body off the request, trims, and caps the
// fields. The byte-cap keeps a malicious client from streaming an unbounded
// body just to fill memory before the JSON parser rejects it.
func decodeAssetPatch(r *http.Request) (assetMetadataPatch, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<10))
	if err != nil {
		return assetMetadataPatch{}, echo.NewHTTPError(http.StatusBadRequest, "read body: "+err.Error())
	}
	var patch assetMetadataPatch
	err = json.Unmarshal(body, &patch)
	if err != nil {
		return assetMetadataPatch{}, echo.NewHTTPError(http.StatusBadRequest, "invalid JSON body")
	}
	patch.Alt = strings.TrimSpace(patch.Alt)
	patch.Description = strings.TrimSpace(patch.Description)
	if len(patch.Alt) > assetMaxAltLen {
		patch.Alt = patch.Alt[:assetMaxAltLen]
	}
	if len(patch.Description) > assetMaxDescriptionLen {
		patch.Description = patch.Description[:assetMaxDescriptionLen]
	}
	return patch, nil
}

// assetDeleteHandler removes a single asset under `{slug}/assets/...`. Mirrors
// the PATCH handler's shape (wildcard path, same validation, same 404-on-
// missing semantics). Snapshots the site before the delete so the History
// panel can restore it. Pages that referenced the image will render a broken
// image until the next edit; the drawer's confirm copy warns about that.
func (s *assetsController) assetDeleteHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	relPath, err := normalizeAssetPath(c.Param("*"))
	if err != nil {
		return err
	}

	obj, err := s.store.Read(c.Request().Context(), slug, relPath)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read asset", err)
	}
	if obj == nil || obj.Content == "" {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}

	s.snapshotBefore(c.Request().Context(), slug, snapshot.ReasonUpload)

	err = s.store.Delete(c.Request().Context(), slug, relPath)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete asset", err)
	}

	return c.JSON(http.StatusOK, map[string]any{"ok": true, "path": relPath}) //nolint:wrapcheck
}

// assetMetadataPatchHandler updates the alt/description metadata on an asset
// without rewriting its bytes. Path is taken from the wildcard route param so
// `assets/photo.png` round-trips intact. Validates slug + path, caps lengths
// to match the vision-captioner, and snapshots the site before the change so
// metadata edits are restorable from the History panel.
func (s *assetsController) assetMetadataPatchHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	relPath, err := normalizeAssetPath(c.Param("*"))
	if err != nil {
		return err
	}
	patch, err := decodeAssetPatch(c.Request())
	if err != nil {
		return err
	}

	// Confirm the object exists before issuing the CopyObject — a PATCH to a
	// missing asset should 404, not implicitly create one.
	obj, err := s.store.Read(c.Request().Context(), slug, relPath)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read asset", err)
	}
	if obj == nil || obj.Content == "" {
		return echo.NewHTTPError(http.StatusNotFound, "asset not found")
	}

	metadata := map[string]string{}
	if patch.Alt != "" {
		metadata["alt"] = patch.Alt
	}
	if patch.Description != "" {
		metadata["description"] = patch.Description
	}

	s.snapshotBefore(c.Request().Context(), slug, snapshot.ReasonUpload)

	err = s.store.UpdateMetadata(c.Request().Context(), slug, relPath, obj.ContentType, metadata)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "update metadata", err)
	}

	return c.JSON(http.StatusOK, assetEntry{ //nolint:wrapcheck
		Path:        relPath,
		URL:         s.siteURL(c, slug, "/"+relPath),
		Alt:         patch.Alt,
		Description: patch.Description,
		ContentType: obj.ContentType,
	})
}

// uploadHandler accepts a multipart file upload from the workspace image
// drawer, enforces the size ceiling, and hands the bytes to the shared
// storeUploadedAsset (sniff + caption + snapshot + write).
func (s *assetsController) uploadHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}

	header, err := c.FormFile("file")
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "file is required")
	}
	if header.Size > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	src, err := header.Open()
	if err != nil {
		return httpErr(http.StatusInternalServerError, "open upload", err)
	}
	defer func() { _ = src.Close() }()

	body, err := io.ReadAll(io.LimitReader(src, maxUploadBytes+1))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if len(body) > maxUploadBytes {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", maxUploadBytes))
	}

	resp, err := s.storeUploadedAsset(c.Request().Context(), slug, header.Filename, body)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp) //nolint:wrapcheck
}
