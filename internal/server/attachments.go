package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/agent"
)

// This file owns reference-attachment intake on /build and /edit/:slug: parsing
// the multipart upload, enforcing the per-file/total/count caps, and
// sanitizing filenames into safe, unique lookup keys. Extracted from server.go.

// maxPromptBodyWithAttachmentsBytes is the outer envelope on /build and
// /edit/:slug where users can attach markdown files alongside the prompt:
// 4 KiB prompt + 512 KiB total attachments + multipart overhead headroom. The
// per-field and per-attachment caps below still gate the actual content.
const maxPromptBodyWithAttachmentsBytes = 768 * 1024

const (
	// maxAttachmentBytes caps each individual markdown attachment. Picked to
	// keep the model's context window manageable when several files are
	// inlined at once.
	maxAttachmentBytes = 64 * 1024
	// maxAttachments caps how many markdown files can ride a single request.
	maxAttachments = 10
	// maxAttachmentsTotalBytes caps the combined size; pairs with the per-file
	// cap so the worst case stays bounded for a small file count too.
	maxAttachmentsTotalBytes = 512 * 1024
)

// parseAttachments pulls user-uploaded reference files (markdown or HTML)
// out of a multipart form on /build and /edit/:slug. Returns nil for "no
// files attached" (which is the common case — the input is optional).
// Validation failures surface as 400s with a readable message; nothing is
// silently dropped.
func parseAttachments(c *echo.Context) ([]agent.Attachment, error) {
	form, err := c.MultipartForm()
	if err != nil {
		// Not a multipart submission (URL-encoded form): treat as "no attachments".
		if errors.Is(err, http.ErrNotMultipart) {
			return nil, nil
		}
		return nil, echo.NewHTTPError(http.StatusBadRequest, "could not parse upload: "+err.Error())
	}
	if form == nil {
		return nil, nil
	}
	files := form.File["attachment"]
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > maxAttachments {
		return nil, echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("too many attachments (max %d)", maxAttachments))
	}

	out := make([]agent.Attachment, 0, len(files))
	seen := make(map[string]int, len(files))
	total := 0
	for _, fh := range files {
		if fh.Size > maxAttachmentBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is too large (max %d bytes)", fh.Filename, maxAttachmentBytes))
		}
		name, err := sanitizeAttachmentName(fh.Filename)
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest, err.Error())
		}
		f, err := fh.Open()
		if err != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("open attachment %q: %s", fh.Filename, err.Error()))
		}
		body, readErr := io.ReadAll(io.LimitReader(f, maxAttachmentBytes+1))
		_ = f.Close()
		if readErr != nil {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("read attachment %q: %s", fh.Filename, readErr.Error()))
		}
		if len(body) > maxAttachmentBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is too large (max %d bytes)", fh.Filename, maxAttachmentBytes))
		}
		if !utf8.Valid(body) {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachment %q is not valid UTF-8 text", fh.Filename))
		}
		total += len(body)
		if total > maxAttachmentsTotalBytes {
			return nil, echo.NewHTTPError(http.StatusBadRequest,
				fmt.Sprintf("attachments exceed combined size limit (%d bytes)", maxAttachmentsTotalBytes))
		}
		// On duplicate basenames, suffix -2, -3, ... so the agent's seed loop
		// still gets distinct lookup keys. Sanitizing collapses more names
		// together (e.g. "a b.md" and "a-b.md" both → "a-b.md"), so loop until
		// the candidate is genuinely unused — including against a literal name
		// a later upload might carry (e.g. someone also uploads "a-b-2.md").
		ext := path.Ext(name)
		stem := strings.TrimSuffix(name, ext)
		uniq := name
		for i := 2; seen[uniq] > 0; i++ {
			uniq = fmt.Sprintf("%s-%d%s", stem, i, ext)
		}
		seen[uniq]++
		out = append(out, agent.Attachment{Name: uniq, Content: string(body)})
	}
	return out, nil
}

// allowedAttachmentExts are the file extensions accepted for reference
// attachments. Markdown for prose source, HTML for existing pages the user
// wants the agent to draw from.
var allowedAttachmentExts = []string{".md", ".markdown", ".html", ".htm"}

// sanitizeAttachmentName returns a safe basename for an uploaded reference
// file. The extension must be one of allowedAttachmentExts (we can't invent
// one), but the rest of the name is sanitized rather than rejected — spaces,
// punctuation, and non-ASCII (e.g. "Caroline & Paweł.html") collapse to
// dashes so the upload just works ("caroline-pawe.html"). Always lowercase
// and limited to [a-z0-9_-] plus the extension.
func sanitizeAttachmentName(raw string) (string, error) {
	base := path.Base(strings.TrimSpace(raw))
	if base == "" || base == "." || base == "/" {
		return "", errors.New("attachment filename is empty")
	}
	lower := strings.ToLower(base)
	ext, ok := matchAttachmentExt(lower)
	if !ok {
		return "", fmt.Errorf("attachment %q must end in %s", raw, strings.Join(allowedAttachmentExts, ", "))
	}
	stem := sanitizeStem(strings.TrimSuffix(lower, ext))
	// Enforce the 80-char total cap by trimming the stem so the extension —
	// which the agent's seed loop keys off — is always preserved.
	if maxStem := 80 - len(ext); len(stem) > maxStem {
		stem = strings.Trim(stem[:maxStem], "-")
		if stem == "" {
			stem = "asset"
		}
	}
	return stem + ext, nil
}

// matchAttachmentExt reports the allowed extension a lowercased name ends in.
func matchAttachmentExt(lower string) (string, bool) {
	for _, ext := range allowedAttachmentExts {
		if strings.HasSuffix(lower, ext) {
			return ext, true
		}
	}
	return "", false
}

// sanitizeStem maps an arbitrary filename stem to a filesystem-safe slug:
// lowercased, every run of characters outside [a-z0-9_-] (spaces, dots,
// punctuation, non-ASCII) collapsed to a single dash, leading/trailing dashes
// trimmed, and an empty result replaced with "asset". Collapsing dots also
// blocks "../" path shenanigans. Shared with the asset-upload path
// (safeAssetName).
func sanitizeStem(stem string) string {
	stem = strings.ToLower(stem)
	var b strings.Builder
	prevDash := false
	for _, r := range stem {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' {
			b.WriteRune(r)
			prevDash = false
			continue
		}
		if !prevDash {
			b.WriteRune('-')
			prevDash = true
		}
	}
	cleaned := strings.Trim(b.String(), "-")
	if cleaned == "" {
		cleaned = "asset"
	}
	return cleaned
}
