package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"

	"github.com/labstack/echo/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/jtarchie/topbanana/internal/auth"
)

// Binary upload over MCP works by ticket, not by stuffing bytes through a JSON
// tool argument: base64 would inflate a 5 MiB image to millions of tokens and,
// worse, land the base64 *text* in the bucket. Instead create_upload_ticket
// mints a short-lived signed URL on Top Banana's own domain and the agent
// curls the raw file there. The ticket handler reuses storeUploadedAsset, so a
// curl'd image gets byte-for-byte the same sniff/allowlist/caption/snapshot
// path as a web upload. The URL points at this app (not S3) so it's reachable
// regardless of whether the bucket endpoint is.

type createUploadTicketInput struct {
	Slug     string `json:"slug"               jsonschema:"The site slug to upload an asset to"`
	Filename string `json:"filename,omitempty" jsonschema:"Optional suggested filename (e.g. logo.png) used only to build the example command; the stored extension is set from the file's actual content."`
}

func (s *Server) registerCreateUploadTicket(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_upload_ticket",
		Description: "Get a short-lived URL to upload a binary image (png/jpg/gif/webp) to a site you own, then curl the file to it — base64 through write_file does not work for binary. SVG and text use write_file directly. The asset is stored under assets/ and best-effort auto-captioned; use the returned path in <img src=\"assets/...\">. Run lint_site afterward.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createUploadTicketInput) (*mcp.CallToolResult, any, error) {
		user, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		token, err := auth.MintUploadTicket(s.mcpSecret, user.Email, in.Slug, auth.UploadTicketTTL, maxUploadBytes)
		if err != nil {
			return nil, nil, fmt.Errorf("mint upload ticket: %w", err)
		}
		uploadURL := s.uploadTicketURL(token)
		return mcpJSON(map[string]any{
			"upload_url":         uploadURL,
			"method":             "POST",
			"max_bytes":          maxUploadBytes,
			"expires_in_seconds": int(auth.UploadTicketTTL.Seconds()),
			"accepted_types":     acceptedAssetTypes(),
			"curl":               uploadTicketCurl(uploadURL, in.Filename),
			"next":               "after uploading, embed the returned path with <img src=\"assets/...\"> via edit_file, then run lint_site",
		})
	})
}

// uploadTicketURL is the public endpoint the agent curls the bytes to. Built
// off mcpBaseURL so it's https in prod / loopback in dev — the app's own
// domain, always reachable by whoever could call the MCP tool.
func (s *Server) uploadTicketURL(token string) string {
	return s.mcpBaseURL() + "/upload/ticket/" + token
}

// uploadTicketCurl is the copy-paste recipe returned to the agent. LOCAL_FILE
// is a placeholder the agent swaps for its file path.
func uploadTicketCurl(uploadURL, filename string) string {
	hint := filename
	if hint == "" {
		hint = "image.png"
	}
	return fmt.Sprintf(`curl -X POST --data-binary @LOCAL_FILE "%s?filename=%s"`, uploadURL, url.QueryEscape(hint))
}

// acceptedAssetTypes lists the sniffed MIME types the upload endpoint accepts,
// sorted for a stable result.
func acceptedAssetTypes() []string {
	out := make([]string, 0, len(allowedAssetTypes))
	for ct := range allowedAssetTypes {
		out = append(out, ct)
	}
	sort.Strings(out)
	return out
}

// uploadTicketHandler accepts the raw binary body an agent curls to a ticket
// URL. The token (in the path) carries the owner, slug, and size cap; we
// re-verify ownership and re-enforce the server's own size ceiling rather than
// trusting the ticket, then hand off to the shared storeUploadedAsset.
func (s *Server) uploadTicketHandler(c *echo.Context) error {
	ticket, err := auth.ParseUploadTicket(s.mcpSecret, c.Param("token"))
	if err != nil {
		return echo.NewHTTPError(http.StatusUnauthorized, "invalid or expired upload ticket")
	}
	slug := ticket.Slug
	// Re-check ownership at upload time (the ticket only proves it was valid at
	// mint): the user might be disabled or the slug reassigned since.
	_, err = s.authorizeSlugOwner(c.Request().Context(), ticket.Email, slug)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "not authorized for this site")
	}

	// Never trust the ticket's cap beyond the server's own ceiling.
	limit := ticket.MaxBytes
	if limit <= 0 || limit > maxUploadBytes {
		limit = maxUploadBytes
	}
	body, err := io.ReadAll(io.LimitReader(c.Request().Body, limit+1))
	if err != nil {
		return httpErr(http.StatusInternalServerError, "read upload", err)
	}
	if int64(len(body)) > limit {
		return echo.NewHTTPError(http.StatusRequestEntityTooLarge, fmt.Sprintf("file exceeds %d bytes", limit))
	}

	resp, err := s.storeUploadedAsset(c.Request().Context(), slug, c.QueryParam("filename"), body)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp) //nolint:wrapcheck
}
