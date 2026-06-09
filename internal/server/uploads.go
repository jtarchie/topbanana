package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// This file owns asset uploads: the shared store-an-image core behind the web
// /upload handler and the MCP upload-ticket handler, content-type sniffing +
// allowlisting, best-effort vision captioning, and safe asset naming.
// Extracted from server.go.

const (
	maxUploadBytes  = 5 << 20 // 5 MiB
	uploadAssetsDir = "assets"
)

// allowedAssetTypes maps sniffed MIME types to a stable file extension we'll
// store under. Keep this restrictive — the agent only knows how to embed
// images via <img>, so we don't accept fonts/video/etc. yet.
var allowedAssetTypes = map[string]string{
	"image/jpeg":    ".jpg",
	"image/png":     ".png",
	"image/gif":     ".gif",
	"image/webp":    ".webp",
	"image/svg+xml": ".svg",
}

type uploadResponse struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	Alt         string `json:"alt,omitempty"`
	Description string `json:"description,omitempty"`
}

// captionTimeout caps how long the upload handler waits on the vision call
// before giving up and storing the asset without metadata. Local models can
// be slow; we'd rather have a usable upload than a hung POST.
const captionTimeout = 90 * time.Second

// storeUploadedAsset is the shared core behind the web /upload handler and the
// MCP upload-ticket handler (mcp_uploads.go). Given the already-read bytes and
// the client's suggested filename, it sniffs + allowlists the content type
// (the extension is forced from the sniff, never trusted), derives a safe
// assets/ path, best-effort captions, snapshots, and writes. Returns an
// echo.HTTPError carrying the right 4xx/5xx status for both callers to surface.
func (s *Server) storeUploadedAsset(ctx context.Context, slug, filename string, body []byte) (uploadResponse, error) {
	contentType := http.DetectContentType(body)
	contentType = strings.SplitN(contentType, ";", 2)[0]
	ext, ok := allowedAssetTypes[contentType]
	if !ok {
		// SVG sniffs as text/xml or text/plain; trust the extension when the
		// upload looks textual.
		if e := strings.ToLower(path.Ext(filename)); e == ".svg" {
			contentType = "image/svg+xml"
			ext = ".svg"
			ok = true
		}
	}
	if !ok {
		return uploadResponse{}, echo.NewHTTPError(http.StatusUnsupportedMediaType, fmt.Sprintf("unsupported type %q (allowed: jpeg, png, gif, webp, svg)", contentType))
	}

	name, err := safeAssetName(filename, ext)
	if err != nil {
		return uploadResponse{}, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	relPath := uploadAssetsDir + "/" + name

	// Caption synchronously so the UI can show the suggested alt-text right
	// next to the upload, and so the agent's first list_assets call already
	// has the metadata. Failures here are non-fatal — the upload still lands.
	caption, captionErr := s.captionUpload(ctx, body, contentType)
	if captionErr != nil {
		slog.Warn("upload.caption_failed", "slug", slug, "path", relPath, "err", captionErr)
	}

	metadata := map[string]string{}
	if caption.Alt != "" {
		metadata["alt"] = caption.Alt
	}
	if caption.Description != "" {
		metadata["description"] = caption.Description
	}

	s.snapshotBefore(ctx, slug, snapshot.ReasonUpload)

	err = s.store.Write(ctx, slug, relPath, string(body), contentType, metadata)
	if err != nil {
		return uploadResponse{}, httpErr(http.StatusInternalServerError, "store asset", err)
	}

	slog.Info("upload.done", "slug", slug, "path", relPath, "type", contentType, "size", len(body), "captioned", caption.Alt != "")
	return uploadResponse{
		Path:        relPath,
		URL:         fmt.Sprintf("http://%s.%s:%s/%s", slug, s.domain, s.port, relPath),
		ContentType: contentType,
		Size:        len(body),
		Alt:         caption.Alt,
		Description: caption.Description,
	}, nil
}

// captionUpload runs the vision sub-agent under a bounded deadline so a slow
// or unresponsive model can't hold the upload request open. The returned
// caption is zero-valued on failure; callers must tolerate that.
func (s *Server) captionUpload(ctx context.Context, body []byte, contentType string) (agent.Caption, error) {
	cctx, cancel := context.WithTimeout(ctx, captionTimeout)
	defer cancel()
	// Resolve the vision-tier model through the build service so captioning
	// shares the same per-model client cache and tier-fallback rules as the
	// rest of the app: an empty LLM_VISION_MODEL falls back to the author
	// model (LLM_MODEL), exactly like the editor/utility tiers. A missing
	// factory (tests) or unresolvable tier yields an error, which the caller
	// logs as a warning — the upload still succeeds without alt-text.
	llm, _, err := s.build.LLMForTier(cctx, model.TierMap{}, model.TierVision)
	if err != nil {
		return agent.Caption{}, fmt.Errorf("resolve vision model: %w", err)
	}
	caption, err := agent.CaptionAsset(cctx, llm, body, contentType)
	if err != nil {
		return caption, fmt.Errorf("caption asset: %w", err)
	}
	return caption, nil
}

// safeAssetName produces a filesystem-safe filename derived from the
// upload's basename, forcing the extension to match the sniffed content
// type. Shares sanitizeStem with attachment names: anything outside
// [a-z0-9_-] collapses to a dash and empty stems become "asset".
func safeAssetName(original, ext string) (string, error) {
	stem := sanitizeStem(strings.TrimSuffix(path.Base(original), path.Ext(original)))
	if len(stem) > 60 {
		stem = strings.Trim(stem[:60], "-")
		if stem == "" {
			stem = "asset"
		}
	}
	return stem + ext, nil
}
