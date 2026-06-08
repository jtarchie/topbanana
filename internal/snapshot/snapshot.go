// Package snapshot captures and restores the full S3 state of a site as a
// single tar+zstd archive. Each user-initiated mutation (build, edit,
// visual-save, settings change, asset upload) snapshots the entire
// `{slug}/` prefix beforehand so the change is reversible. Restores
// auto-snapshot first, so restoring is itself reversible.
package snapshot

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/jtarchie/topbanana/internal/store"
)

// snapshotPrefix is the bucket-level prefix under which all archives live.
// Sits outside any user-slug namespace (leading underscore is reserved by
// slug validation) so archives never appear in per-site listings or in
// subdomain routing.
const snapshotPrefix = "_snapshots/"

const archiveContentType = "application/zstd"

// PAX header key prefixes for per-file content type and S3 user metadata.
// New archives are always written with the TOPBANANA.* prefix. Exported so
// internal/portable can produce archives in the same wire format.
const (
	PAXContentTypeKey = "TOPBANANA.content-type"
	PAXMetaPrefix     = "TOPBANANA.meta."
)

// Legacy PAX prefixes from before each rebrand, newest first. ContentTypeFromPAX
// and MetadataFromPAX fall through these so archives written before a rebrand
// still restore cleanly: BLOOMHOLLOW.* predates Top Banana, BUILDABEAR.*
// predates Bloomhollow.
var (
	LegacyPAXContentTypeKeys = []string{"BLOOMHOLLOW.content-type", "BUILDABEAR.content-type"}
	LegacyPAXMetaPrefixes    = []string{"BLOOMHOLLOW.meta.", "BUILDABEAR.meta."}
)

// ContentTypeFromPAX returns the per-file content type recorded in a tar
// header's PAX records, checking the current key then each legacy key in
// order. Returns "" when none is present (caller should fall back to the file
// extension).
func ContentTypeFromPAX(rec map[string]string) string {
	if ct := rec[PAXContentTypeKey]; ct != "" {
		return ct
	}
	for _, k := range LegacyPAXContentTypeKeys {
		if ct := rec[k]; ct != "" {
			return ct
		}
	}
	return ""
}

// MetadataFromPAX extracts S3 user metadata from a tar header's PAX records,
// honouring the current meta prefix and each legacy prefix. Returns nil when
// no metadata records are present.
func MetadataFromPAX(rec map[string]string) map[string]string {
	var metadata map[string]string
	prefixes := append([]string{PAXMetaPrefix}, LegacyPAXMetaPrefixes...)
	for k, v := range rec {
		for _, p := range prefixes {
			rest, ok := strings.CutPrefix(k, p)
			if !ok {
				continue
			}
			if metadata == nil {
				metadata = map[string]string{}
			}
			metadata[rest] = v
			break
		}
	}
	return metadata
}

// Reasons that show up in the History UI. Free-form strings — these constants
// just keep callers consistent.
const (
	ReasonBuild      = "build"
	ReasonEdit       = "edit"
	ReasonVisualSave = "visual-save"
	ReasonThemeApply = "theme-apply"
	ReasonSettings   = "settings"
	ReasonUpload     = "upload"
	ReasonPreRestore = "pre-restore"
)

// Snapshot is one archive's metadata.
type Snapshot struct {
	Key       string
	Timestamp time.Time
	Reason    string
	SizeBytes int64
	FileCount int
}

// Service creates, lists, restores, and deletes archives. Stateless apart
// from the Store handle and the retention cap.
type Service struct {
	store *store.Store
	// keep caps the number of archives retained per slug. 0 disables retention.
	keep int
}

func New(s *store.Store, keep int) *Service {
	return &Service{store: s, keep: keep}
}

// Create snapshots every object under `{slug}/` into one tar+zstd archive at
// `_snapshots/{slug}/{timestamp}-{reason}.tar.zst`. After a successful write,
// trims old archives down to the retention cap.
func (s *Service) Create(ctx context.Context, slug, reason string) (Snapshot, error) {
	if slug == "" {
		return Snapshot{}, errors.New("snapshot: slug is empty")
	}
	if reason == "" {
		reason = "manual"
	}

	files, err := s.store.List(ctx, slug)
	if err != nil {
		return Snapshot{}, fmt.Errorf("list slug %s: %w", slug, err)
	}

	now := time.Now().UTC()
	archive, err := buildArchive(ctx, s.store, slug, files, now)
	if err != nil {
		return Snapshot{}, err
	}

	key := snapshotKey(slug, now, reason)
	metadata := map[string]string{
		"reason":        reason,
		"file-count":    strconv.Itoa(archive.FileCount),
		"original-size": strconv.FormatInt(archive.OriginalBytes, 10),
		"created":       now.Format(time.RFC3339),
	}
	err = s.store.WriteRaw(ctx, key, archive.Content, archiveContentType, metadata)
	if err != nil {
		return Snapshot{}, fmt.Errorf("write archive %s: %w", key, err)
	}

	snap := Snapshot{
		Key:       key,
		Timestamp: now,
		Reason:    reason,
		SizeBytes: int64(len(archive.Content)),
		FileCount: archive.FileCount,
	}

	slog.Info("snapshot.create", "slug", slug, "reason", reason, "files", archive.FileCount, "archive_bytes", len(archive.Content), "original_bytes", archive.OriginalBytes)

	if s.keep > 0 {
		s.trim(ctx, slug)
	}

	return snap, nil
}

// List returns every snapshot for a slug, newest first.
func (s *Service) List(ctx context.Context, slug string) ([]Snapshot, error) {
	prefix := snapshotPrefix + slug + "/"
	keys, err := s.store.ListPrefix(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("list snapshots %s: %w", slug, err)
	}

	out := make([]Snapshot, 0, len(keys))
	for _, key := range keys {
		obj, err := s.store.ReadRaw(ctx, key)
		if err != nil {
			slog.Warn("snapshot.read_metadata_failed", "key", key, "err", err)
			continue
		}
		snap := Snapshot{
			Key:       key,
			SizeBytes: int64(len(obj.Content)),
		}
		if v := obj.Metadata["reason"]; v != "" {
			snap.Reason = v
		}
		if v := obj.Metadata["file-count"]; v != "" {
			var n int
			_, _ = fmt.Sscanf(v, "%d", &n)
			snap.FileCount = n
		}
		if v := obj.Metadata["created"]; v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err == nil {
				snap.Timestamp = t
			}
		}
		if snap.Timestamp.IsZero() {
			snap.Timestamp = timestampFromKey(key)
		}
		out = append(out, snap)
	}

	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out, nil
}

// Restore wipes the slug's current state and extracts the named archive over
// it. Auto-snapshots first as "pre-restore" so the restore itself is
// reversible.
func (s *Service) Restore(ctx context.Context, slug, key string) error {
	if !strings.HasPrefix(key, snapshotPrefix+slug+"/") {
		return fmt.Errorf("snapshot key %q does not belong to slug %q", key, slug)
	}

	_, err := s.Create(ctx, slug, ReasonPreRestore)
	if err != nil {
		return fmt.Errorf("pre-restore snapshot: %w", err)
	}

	obj, err := s.store.ReadRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("read archive %s: %w", key, err)
	}
	if obj.Content == "" {
		return fmt.Errorf("snapshot %s is empty or missing", key)
	}

	current, err := s.store.List(ctx, slug)
	if err != nil {
		return fmt.Errorf("list current files: %w", err)
	}
	for _, p := range current {
		err := s.store.Delete(ctx, slug, p)
		if err != nil {
			return fmt.Errorf("wipe %s/%s: %w", slug, p, err)
		}
	}

	return ExtractArchive(ctx, s.store, slug, obj.Content)
}

// Delete removes a single archive. Doesn't touch site state.
func (s *Service) Delete(ctx context.Context, slug, key string) error {
	if !strings.HasPrefix(key, snapshotPrefix+slug+"/") {
		return fmt.Errorf("snapshot key %q does not belong to slug %q", key, slug)
	}
	err := s.store.DeleteRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("delete archive %s: %w", key, err)
	}
	return nil
}

// trim deletes the oldest archives beyond the retention cap. Logged-only on
// failure — retention is opportunistic and shouldn't fail a snapshot.
func (s *Service) trim(ctx context.Context, slug string) {
	snaps, err := s.List(ctx, slug)
	if err != nil {
		slog.Warn("snapshot.trim_list_failed", "slug", slug, "err", err)
		return
	}
	if len(snaps) <= s.keep {
		return
	}
	for _, victim := range snaps[s.keep:] {
		err := s.store.DeleteRaw(ctx, victim.Key)
		if err != nil {
			slog.Warn("snapshot.trim_delete_failed", "slug", slug, "key", victim.Key, "err", err)
			continue
		}
		slog.Info("snapshot.trim", "slug", slug, "key", victim.Key)
	}
}

// archiveResult is what buildArchive produces: the tar+zstd bytes plus the
// file count and total uncompressed payload size that Create stamps into the
// snapshot metadata.
type archiveResult struct {
	Content       string
	FileCount     int
	OriginalBytes int64
}

// buildArchive streams every slug-relative file through tar+zstd, returning the
// archive bytes plus the file count and uncompressed payload size for metadata.
func buildArchive(ctx context.Context, st *store.Store, slug string, files []string, ts time.Time) (archiveResult, error) {
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		return archiveResult{}, fmt.Errorf("init zstd: %w", err)
	}
	tw := tar.NewWriter(zw)

	var originalBytes int64
	count := 0
	for _, p := range files {
		obj, err := st.Read(ctx, slug, p)
		if err != nil {
			return archiveResult{}, fmt.Errorf("read %s/%s: %w", slug, p, err)
		}
		body := []byte(obj.Content)
		header := &tar.Header{
			Name:    p,
			Size:    int64(len(body)),
			Mode:    0644,
			ModTime: ts,
		}
		if obj.ContentType != "" {
			header.PAXRecords = map[string]string{PAXContentTypeKey: obj.ContentType}
		}
		if len(obj.Metadata) > 0 {
			if header.PAXRecords == nil {
				header.PAXRecords = map[string]string{}
			}
			for k, v := range obj.Metadata {
				header.PAXRecords[PAXMetaPrefix+k] = v
			}
		}
		err = tw.WriteHeader(header)
		if err != nil {
			return archiveResult{}, fmt.Errorf("tar header %s: %w", p, err)
		}
		_, err = tw.Write(body)
		if err != nil {
			return archiveResult{}, fmt.Errorf("tar write %s: %w", p, err)
		}
		originalBytes += int64(len(body))
		count++
	}

	err = tw.Close()
	if err != nil {
		return archiveResult{}, fmt.Errorf("close tar: %w", err)
	}
	err = zw.Close()
	if err != nil {
		return archiveResult{}, fmt.Errorf("close zstd: %w", err)
	}
	return archiveResult{Content: buf.String(), FileCount: count, OriginalBytes: originalBytes}, nil
}

// ExtractArchive decodes the tar+zstd payload and writes each entry back
// under `{slug}/`. Content type and metadata are restored from PAX records
// when present; otherwise the content type is sniffed from the path.
func ExtractArchive(ctx context.Context, st *store.Store, slug, archive string) error {
	zr, err := zstd.NewReader(strings.NewReader(archive))
	if err != nil {
		return fmt.Errorf("open zstd: %w", err)
	}
	defer zr.Close()

	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("tar next: %w", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			return fmt.Errorf("tar read %s: %w", hdr.Name, err)
		}

		contentType := ContentTypeFromPAX(hdr.PAXRecords)
		if contentType == "" {
			if ext := path.Ext(hdr.Name); ext != "" {
				contentType = mime.TypeByExtension(ext)
			}
		}

		metadata := MetadataFromPAX(hdr.PAXRecords)

		err = st.Write(ctx, slug, hdr.Name, string(body), contentType, metadata)
		if err != nil {
			return fmt.Errorf("write %s/%s: %w", slug, hdr.Name, err)
		}
	}
}

// snapshotKey assembles the bucket key for a new archive. Format:
// `_snapshots/{slug}/20260513T142233Z-build.tar.zst`. The compact RFC3339
// basic form sorts lexicographically.
func snapshotKey(slug string, ts time.Time, reason string) string {
	return fmt.Sprintf("%s%s/%s-%s.tar.zst", snapshotPrefix, slug, ts.Format("20060102T150405Z"), reason)
}

// timestampFromKey parses the timestamp out of a key whose metadata is
// missing or corrupted. Best-effort fallback used by List.
func timestampFromKey(key string) time.Time {
	base := path.Base(key)
	// {timestamp}-{reason}.tar.zst
	idx := strings.IndexByte(base, '-')
	if idx <= 0 {
		return time.Time{}
	}
	t, err := time.Parse("20060102T150405Z", base[:idx])
	if err != nil {
		return time.Time{}
	}
	return t
}
