// Package portable encodes and decodes the cross-instance site archive used
// by the workspace Export tool and the landing-page Import flow.
//
// The wire format is a tar+zstd stream with the same PAX records as
// internal/archive — content-type and S3 user metadata travel inside each
// entry — plus one synthetic `topbanana-export.json` entry at the top that
// captures the template id, title, and description from the source site's
// SiteMeta. The full `.topbanana.json` sidecar is intentionally *not*
// included: OwnerID, Domains, EnablesPublicAPI, and Created are
// instance-specific and must be re-derived from the importing user on the
// destination instance.
//
// Import enforces three independent caps — compressed archive size, file
// count, and post-decompress total bytes — so a hand-crafted archive cannot
// exhaust memory or disk on the destination.
package portable

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"path"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/archive"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/store"
)

const (
	ArchiveContentType = "application/zstd"
	ArchiveExt         = ".tar.zst"
	ManifestPath       = "topbanana-export.json"
	ManifestVersion    = 1

	// legacyManifestPath is the pre-Top-Banana manifest entry name. Imports
	// recognise it so archives exported before the rebrand still restore.
	legacyManifestPath = "bloomhollow-export.json"

	MaxArchiveBytes   = 50 << 20  // 50 MiB compressed
	MaxExtractedBytes = 100 << 20 // 100 MiB uncompressed (zip-bomb guard)
	MaxFileCount      = 5000
)

// stateDirPrefix is the per-site KV-state directory that lives under
// `{slug}/_state/`. It holds form-submission data scoped to the source
// instance; an importer should never inherit that history. Defined in the
// store keyspace registry (store.StateDir) so every reserved area lives in
// one place.
const stateDirPrefix = store.StateDir

// reservedPaths are top-level entries inside `{slug}/` that the import path
// silently drops even if a hand-crafted archive contains them. Defense in
// depth — the export-side filter already excludes them, but the import side
// cannot trust the archive.
var reservedPaths = map[string]bool{
	build.MetaFile:      true,
	".bloomhollow.json": true, // legacy meta name (pre Top Banana)
	".buildabear.json":  true, // legacy meta name (pre Bloomhollow)
	ManifestPath:        true, // handled separately as the manifest entry
	legacyManifestPath:  true, // legacy manifest, also handled separately
}

// Manifest is the synthetic `topbanana-export.json` entry shipped at the
// top of every archive. Unknown future fields are tolerated by the importer.
type Manifest struct {
	Version     int       `json:"version"`
	Template    string    `json:"template,omitempty"`
	Title       string    `json:"title,omitempty"`
	Description string    `json:"description,omitempty"`
	ExportedAt  time.Time `json:"exported_at"`
}

// ImportResult carries the manifest fields the handler needs to synthesize a
// fresh SiteMeta on the destination, plus a file count for logging.
type ImportResult struct {
	Template    string
	Title       string
	Description string
	FileCount   int
}

var (
	ErrArchiveTooLarge = errors.New("portable: archive exceeds size cap")
	ErrTooManyFiles    = errors.New("portable: archive exceeds file count cap")
	ErrNoIndex         = errors.New("portable: archive missing index.html")
	ErrCorrupt         = errors.New("portable: archive is not a valid tar.zst")
	ErrExtractedTooBig = errors.New("portable: archive contents exceed uncompressed cap")
)

// Export reads every object under `{slug}/` (skipping the meta sidecar and
// the `_state/` subtree), prepends a Manifest entry, and returns a tar+zstd
// archive. SiteMeta is supplied by the caller so this package doesn't have
// to depend on the meta read path.
func Export(ctx context.Context, st *store.Store, slug string, meta build.SiteMeta) ([]byte, error) {
	if slug == "" {
		return nil, errors.New("portable: slug is empty")
	}

	files, err := st.List(ctx, slug)
	if err != nil {
		return nil, fmt.Errorf("list slug %s: %w", slug, err)
	}

	now := time.Now().UTC()
	manifest := Manifest{
		Version:     ManifestVersion,
		Template:    meta.Template,
		Title:       meta.Title,
		Description: meta.Description,
		ExportedAt:  now,
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal manifest: %w", err)
	}

	var buf bytes.Buffer
	aw, err := archive.NewWriter(&buf)
	if err != nil {
		return nil, err //nolint:wrapcheck // archive.NewWriter already contextualizes
	}

	err = aw.WriteFile(ManifestPath, manifestBytes, "application/json", nil, now)
	if err != nil {
		return nil, fmt.Errorf("write manifest entry: %w", err)
	}

	for _, p := range files {
		if shouldSkipOnExport(p) {
			continue
		}
		obj, readErr := st.Read(ctx, slug, p)
		if readErr != nil {
			return nil, fmt.Errorf("read %s/%s: %w", slug, p, readErr)
		}
		if obj.Content == "" {
			continue
		}
		writeErr := aw.WriteFile(p, []byte(obj.Content), obj.ContentType, obj.Metadata, now)
		if writeErr != nil {
			return nil, fmt.Errorf("tar write %s: %w", p, writeErr)
		}
	}

	err = aw.Close()
	if err != nil {
		return nil, err //nolint:wrapcheck // archive.Close already contextualizes
	}
	return buf.Bytes(), nil
}

// Import validates `archive` against the size/count caps, decompresses it,
// and writes each non-reserved entry under `{slug}/`. Returns the manifest
// fields so the caller can build a fresh SiteMeta. On error, the slug may
// hold a partial extract — the handler is responsible for cleanup, because
// only the handler knows whether the slug was already in the index.
//
//nolint:cyclop // many short-circuit branches are exactly the validation surface this function exists for.
func Import(ctx context.Context, st *store.Store, slug string, data []byte) (ImportResult, error) {
	if slug == "" {
		return ImportResult{}, errors.New("portable: slug is empty")
	}
	if len(data) > MaxArchiveBytes {
		return ImportResult{}, ErrArchiveTooLarge
	}

	// Cap the total bytes the tar reader will see post-decompression so a
	// small compressed archive cannot fan out into gigabytes of memory.
	rd, err := archive.NewReader(bytes.NewReader(data), MaxExtractedBytes+1)
	if err != nil {
		return ImportResult{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
	}
	defer rd.Close()
	tr := rd.Reader

	var (
		manifest    Manifest
		result      ImportResult
		totalBytes  int64
		hasIndex    bool
		seenEntries int
	)

	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ImportResult{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA { //nolint:staticcheck // TypeRegA preserved for legacy tar producers
			// Directory entries and symlinks are not part of our wire format;
			// silently skip rather than fail so an oddly-built archive still
			// imports its regular files.
			continue
		}

		seenEntries++
		if seenEntries > MaxFileCount {
			return ImportResult{}, ErrTooManyFiles
		}

		body, err := io.ReadAll(tr)
		if err != nil {
			return ImportResult{}, fmt.Errorf("%w: %w", ErrCorrupt, err)
		}
		totalBytes += int64(len(body))
		if totalBytes > MaxExtractedBytes {
			return ImportResult{}, ErrExtractedTooBig
		}

		name := cleanArchiveName(hdr.Name)
		if name == "" {
			continue
		}

		if name == ManifestPath || name == legacyManifestPath {
			applyManifest(body, &manifest, &result)
			continue
		}

		if reservedPaths[name] || strings.HasPrefix(name, stateDirPrefix) {
			// Defense in depth: never let an archive plant meta or
			// per-site state on the destination.
			continue
		}

		contentType, metadata := decodePAX(hdr, name)
		err = st.Write(ctx, slug, name, string(body), contentType, metadata)
		if err != nil {
			return ImportResult{}, fmt.Errorf("write %s/%s: %w", slug, name, err)
		}
		result.FileCount++
		if name == "index.html" {
			hasIndex = true
		}
	}

	if !hasIndex {
		return ImportResult{}, ErrNoIndex
	}
	return result, nil
}

// Cleanup removes every object under `{slug}/`. The import handler calls
// this on extraction failure so a partially-written slug doesn't outlive the
// failed request. Errors are logged by the caller — best-effort by design.
func Cleanup(ctx context.Context, st *store.Store, slug string) error {
	files, err := st.List(ctx, slug)
	if err != nil {
		return fmt.Errorf("list slug %s: %w", slug, err)
	}
	for _, p := range files {
		delErr := st.Delete(ctx, slug, p)
		if delErr != nil {
			return fmt.Errorf("delete %s/%s: %w", slug, p, delErr)
		}
	}
	return nil
}

// shouldSkipOnExport drops the instance-specific meta sidecars and the
// `_state/` subtree from the archive so an importing instance cannot
// inherit OwnerID, custom domains, or another user's form submissions.
func shouldSkipOnExport(p string) bool {
	if p == build.MetaFile || p == ".bloomhollow.json" || p == ".buildabear.json" {
		return true
	}
	if strings.HasPrefix(p, stateDirPrefix) {
		return true
	}
	return false
}

// applyManifest parses the synthetic manifest entry and copies its fields
// into the ImportResult. A malformed manifest is tolerated — unknown future
// versions and corrupt JSON still let the rest of the archive import.
func applyManifest(body []byte, manifest *Manifest, result *ImportResult) {
	err := json.Unmarshal(body, manifest)
	if err != nil {
		return
	}
	result.Template = manifest.Template
	result.Title = manifest.Title
	result.Description = manifest.Description
}

// decodePAX extracts the content type and user metadata from a tar entry's
// PAX records, falling back through the legacy prefixes and to a
// MIME-by-extension lookup when the archive doesn't carry the content type
// explicitly.
func decodePAX(hdr *tar.Header, name string) (string, map[string]string) {
	contentType := archive.ContentTypeFromPAX(hdr.PAXRecords)
	if contentType == "" {
		if ext := path.Ext(name); ext != "" {
			contentType = mime.TypeByExtension(ext)
		}
	}
	return contentType, archive.MetadataFromPAX(hdr.PAXRecords)
}

// cleanArchiveName trims a leading "./" and rejects names that try to escape
// the slug prefix via "..", absolute paths, or backslash separators. The
// store's own validateObjectPath would reject these on Write, but bailing
// here gives a clearer error and a faster failure.
func cleanArchiveName(name string) string {
	name = strings.TrimPrefix(name, "./")
	if name == "" {
		return ""
	}
	if strings.HasPrefix(name, "/") || strings.Contains(name, `\`) {
		return ""
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return ""
		}
	}
	return name
}
