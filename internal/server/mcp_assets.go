package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// Uploaded images, as the MCP surface sees them. create_upload_ticket could
// already put an image on a site, but nothing could read back what was there —
// so an MCP-authored site's alt text was write-once and invisible, an
// accessibility gap the agent that created it couldn't even detect. The
// in-process build agent has had list_assets since the beginning
// (internal/agent); this is the same view for the external one, plus the
// metadata write the workspace image drawer performs.

// mcpAssetEntry mirrors assetEntry (the drawer's JSON row) with times shaped
// for a tool result. Deliberately the same field names so a caller reading
// both surfaces sees one vocabulary.
type mcpAssetEntry struct {
	Path        string `json:"path"`
	URL         string `json:"url"`
	Alt         string `json:"alt,omitempty"`
	Description string `json:"description,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
	Modified    string `json:"modified,omitempty"`
}

// mcpAssetPath normalizes and validates a caller-supplied asset path: the
// `assets/` prefix is optional (an agent that read a page's <img src> has the
// bare name), traversal is refused. Mirrors normalizeAssetPath but returns a
// plain error — an echo.HTTPError renders as "code=400, message=..." inside a
// tool result, which reads like a bug to the agent receiving it.
func mcpAssetPath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "/")
	if raw == "" {
		return "", errors.New("path is required")
	}
	if !strings.HasPrefix(raw, uploadAssetsDir+"/") {
		raw = uploadAssetsDir + "/" + raw
	}
	if strings.Contains(raw, "..") {
		return "", fmt.Errorf("invalid asset path %q", raw)
	}
	return raw, nil
}

type listAssetsInput struct {
	Slug string `json:"slug" jsonschema:"The site slug"`
}

func (s *Server) registerListAssets(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_assets",
		Description: "List the uploaded images on a site the caller owns (everything under assets/), with each one's public URL, alt text, description, size, and last-modified time. Use it to find an image to reference from a page, or to audit which uploads are still missing alt text — list_files only reports bare paths.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in listAssetsInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		entries, err := s.store.ListWithMeta(ctx, in.Slug)
		if err != nil {
			return nil, nil, fmt.Errorf("list assets: %w", err)
		}
		out := make([]mcpAssetEntry, 0, len(entries))
		missingAlt := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Path, uploadAssetsDir+"/") {
				continue
			}
			row := mcpAssetEntry{
				Path: e.Path,
				URL:  s.mcpPageURL(in.Slug, e.Path),
				Size: e.Size,
			}
			if !e.LastModified.IsZero() {
				row.Modified = e.LastModified.UTC().Format(time.RFC3339)
			}
			// Metadata rides on the object, so it takes a Read per asset; the
			// store's cache absorbs the repeat within a request, same as the
			// drawer's list handler.
			obj, readErr := s.store.Read(ctx, in.Slug, e.Path)
			if readErr == nil && obj != nil {
				row.Alt = obj.Metadata["alt"]
				row.Description = obj.Metadata["description"]
				row.ContentType = obj.ContentType
			}
			if row.Alt == "" {
				missingAlt++
			}
			out = append(out, row)
		}
		sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })

		res := map[string]any{"slug": in.Slug, "assets": out}
		if missingAlt > 0 {
			res["next"] = fmt.Sprintf(
				"%d asset(s) have no alt text — screen readers announce nothing for them; set it with set_asset_metadata", missingAlt)
		}
		return mcpJSON(res)
	})
}

// setAssetMetadataInput takes pointers for the two editable fields, matching
// configure_site: only what you pass is changed. The store replaces an
// object's metadata wholesale, so a plain string would make an omitted
// `description` indistinguishable from an explicit clear — and quietly delete
// the caption the vision model wrote at upload time.
type setAssetMetadataInput struct {
	Slug        string  `json:"slug"                  jsonschema:"The site slug"`
	Path        string  `json:"path"                  jsonschema:"Asset path, with or without the assets/ prefix (e.g. assets/hero.png or hero.png)"`
	Alt         *string `json:"alt,omitempty"         jsonschema:"Alt text: what a screen reader announces in place of the image. One short phrase describing the content, not the file. Omit to leave unchanged; pass an empty string to clear. Truncated at 125 characters."`
	Description *string `json:"description,omitempty" jsonschema:"Longer internal description of the image, for picking it out of a list later. Not rendered on the page. Omit to leave unchanged; pass an empty string to clear. Truncated at 500 characters."`
}

func (s *Server) registerSetAssetMetadata(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_asset_metadata",
		Description: "Set the alt text and description on an uploaded image in a site the caller owns. Alt text is what screen readers announce, so every image that carries meaning needs one. Only the fields you pass are changed — omit one to leave it as it is, or pass an empty string to clear it. Does not re-upload the image.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setAssetMetadataInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		relPath, err := mcpAssetPath(in.Path)
		if err != nil {
			return nil, nil, err
		}
		// Confirm the object exists first: replaceMeta is a CopyObject, and on a
		// missing key that would either fail obscurely or conjure an empty
		// object. Same 404-shaped guard as the web PATCH handler.
		obj, err := s.store.Read(ctx, in.Slug, relPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read asset %q: %w", relPath, err)
		}
		if obj == nil || obj.Content == "" {
			return nil, nil, fmt.Errorf("asset %q not found (list_assets shows what is there)", relPath)
		}

		// Start from what's stored and overlay only the fields that were sent,
		// since UpdateMetadata replaces rather than merges.
		alt := obj.Metadata["alt"]
		description := obj.Metadata["description"]
		if in.Alt != nil {
			alt = mcpTruncate(strings.TrimSpace(*in.Alt), assetMaxAltLen)
		}
		if in.Description != nil {
			description = mcpTruncate(strings.TrimSpace(*in.Description), assetMaxDescriptionLen)
		}
		metadata := map[string]string{}
		if alt != "" {
			metadata["alt"] = alt
		}
		if description != "" {
			metadata["description"] = description
		}

		s.snapshotBefore(ctx, in.Slug, snapshot.ReasonUpload)
		err = s.store.UpdateMetadata(ctx, in.Slug, relPath, obj.ContentType, metadata)
		if err != nil {
			return nil, nil, fmt.Errorf("update metadata: %w", err)
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "path": relPath,
			"alt": alt, "description": description,
			"url": s.mcpPageURL(in.Slug, relPath),
		})
	})
}

// mcpTruncate caps a field at n bytes, backing up to a rune boundary so a
// clipped string stays valid UTF-8. The caps are byte counts (they bound an S3
// metadata header), and a naive s[:n] through the middle of a multi-byte
// character would store a replacement char the owner then has to notice.
func mcpTruncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
