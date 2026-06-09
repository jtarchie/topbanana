package store

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/hashicorp/golang-lru/arc/v2"

	"github.com/jtarchie/topbanana/internal/compressutil"
)

// Store is the platform's object-storage abstraction. It owns the cross-cutting
// rules — compression at rest, slug-prefix path validation, metadata
// URL-encoding, and the ARC read cache — and delegates the actual byte movement
// to an objectBackend (S3 in production, an in-memory map in tests). See
// backend.go for the seam.
type Store struct {
	backend objectBackend
	cache   *arc.ARCCache[string, *S3Object]
}

// New returns a Store backed by a real S3 bucket via client.
func New(client *s3.Client, bucket string, cacheSize int) (*Store, error) {
	s := &Store{backend: &s3Backend{client: client, bucket: bucket}}
	err := s.initCache(cacheSize)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// NewInMemory returns a Store backed by an in-process map instead of S3. It runs
// the same compression, path-validation, metadata-encoding, and caching logic as
// New, so tests across the storage layer (and everything built on it — editrec,
// snapshot, portable, build, auth) get deterministic coverage without a live
// Minio. cacheSize behaves as in New (<= 0 disables the ARC cache).
func NewInMemory(cacheSize int) (*Store, error) {
	s := &Store{backend: newMemoryBackend()}
	err := s.initCache(cacheSize)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) initCache(cacheSize int) error {
	if cacheSize <= 0 {
		return nil
	}
	cache, err := arc.NewARC[string, *S3Object](cacheSize)
	if err != nil {
		return fmt.Errorf("failed to create ARC cache: %w", err)
	}
	s.cache = cache
	return nil
}

type S3Object struct {
	Content     string
	ETag        string
	ContentType string
	// Metadata is user-defined key/value pairs stored as x-amz-meta-* headers
	// on the object. Values are URL-decoded on read so callers see plain
	// unicode; the store handles encoding on write because S3 metadata must
	// be ASCII.
	Metadata map[string]string
}

const DefaultContentType = "text/html; charset=utf-8"

// isCompressibleContentType reports whether a written object should be
// zstd-compressed at rest. The allowlist covers everything text-y the platform
// stores (HTML pages, the compiled app.css, JSON sidecars, inline JS for MCP
// functions, hand-authored SVGs) and excludes pre-compressed binary blobs
// (JPEG/PNG/GIF/WEBP, fonts) — re-compressing those wastes CPU and usually
// grows them slightly. Read sniffs the stored bytes (compressutil.HasMagic) so
// the on-write decision is purely about which content types pay the CPU to be
// compressed; switching the policy doesn't require a migration.
func isCompressibleContentType(contentType string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch ct {
	case "application/json",
		"application/javascript",
		"application/xml",
		"application/xhtml+xml",
		"image/svg+xml":
		return true
	}
	return false
}

func (s *Store) Write(ctx context.Context, slug, path, content, contentType string, metadata map[string]string) error {
	err := ValidateObjectPath(path)
	if err != nil {
		return err
	}
	key := slug + "/" + path
	if contentType == "" {
		contentType = DefaultContentType
	}

	// S3 user-defined metadata must be ASCII. URL-encode so unicode (e.g. an
	// alt-text with em-dashes or non-Latin characters) round-trips safely.
	encoded := encodeMetadata(metadata)

	// Compress text-y payloads at rest. ContentType stays unchanged because
	// callers (proxy, agent, MCP, lint) consume the decompressed form via
	// Read — the stored representation is an implementation detail of this
	// package, not part of the external object contract.
	body := content
	if isCompressibleContentType(contentType) {
		compressed, cerr := compressutil.Compress([]byte(content))
		if cerr != nil {
			return fmt.Errorf("failed to compress object %s: %w", key, cerr)
		}
		body = string(compressed)
	}

	etag, err := s.backend.put(ctx, key, []byte(body), contentType, encoded)
	if err != nil {
		return err
	}

	if s.cache != nil {
		s.cache.Add(key, &S3Object{
			Content:     content,
			ETag:        etag,
			ContentType: contentType,
			Metadata:    cloneMetadata(metadata),
		})
	}

	return nil
}

// encodeMetadata URL-escapes metadata values so unicode round-trips through
// S3's ASCII-only headers. Returns nil for empty input (so the backend stores
// no metadata at all rather than an empty map).
func encodeMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	encoded := make(map[string]string, len(metadata))
	for k, v := range metadata {
		encoded[k] = url.QueryEscape(v)
	}
	return encoded
}

// decodeMetadata reverses encodeMetadata: lowercases keys (S3 normalizes header
// keys) and URL-unescapes values, falling back to the raw value if it isn't
// valid escaping (a pre-encoding object).
func decodeMetadata(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		dec, decErr := url.QueryUnescape(v)
		if decErr != nil {
			dec = v
		}
		out[strings.ToLower(k)] = dec
	}
	return out
}

func cloneMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ValidateObjectPath rejects relative-traversal segments, absolute paths, and
// Windows separators. The proxy handler already gates on this before reaching
// here, but every caller of Read/Write benefits from the same check —
// otherwise a future agent tool or handler could write objects at keys like
// `slug/../other/...` that escape the per-tenant prefix. Exported so the server
// proxy/validatePage path can share this exact rule instead of re-deriving it.
func ValidateObjectPath(path string) error {
	if strings.Contains(path, "..") || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
		return fmt.Errorf("invalid object path %q", path)
	}
	return nil
}

func (s *Store) Read(ctx context.Context, slug, path string) (*S3Object, error) {
	err := ValidateObjectPath(path)
	if err != nil {
		return nil, err
	}
	key := slug + "/" + path

	if s.cache != nil {
		if cached, ok := s.cache.Get(key); ok {
			slog.Debug("store.cache", "slug", slug, "path", path, "hit", true)
			return cached, nil
		}
		slog.Debug("store.cache", "slug", slug, "path", path, "hit", false)
	}

	raw, err := s.backend.get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get object %s/%s: %w", slug, path, err)
	}
	if raw == nil {
		// Clean miss: cache an empty object so repeat lookups don't re-hit the
		// backend, matching the prior NoSuchKey behaviour.
		obj := &S3Object{}
		if s.cache != nil {
			s.cache.Add(key, obj)
		}
		return obj, nil
	}

	// Sniff zstd magic and decompress. Pre-compression objects (raw HTML, CSS,
	// JSON written before this package learned to compress) pass through
	// unchanged. No metadata flag needed because the magic bytes don't collide
	// with anything the platform stores in plaintext.
	payload, err := compressutil.MaybeDecompress(raw.body)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress object %s/%s: %w", slug, path, err)
	}

	obj := &S3Object{
		Content:     string(payload),
		ETag:        raw.etag,
		ContentType: raw.contentType,
		Metadata:    decodeMetadata(raw.metadata),
	}

	if s.cache != nil {
		s.cache.Add(key, obj)
	}

	return obj, nil
}

func (s *Store) List(ctx context.Context, slug string) ([]string, error) {
	prefix := slug + "/"
	infos, err := s.backend.list(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects in %s: %w", slug, err)
	}
	files := make([]string, 0, len(infos))
	for _, info := range infos {
		name := strings.TrimPrefix(info.key, prefix)
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// FileEntry is one row of ListWithMeta — the data the file explorer renders.
// LastModified comes straight from the backend and is already UTC.
type FileEntry struct {
	Path         string
	Size         int64
	LastModified time.Time
}

// ListWithMeta is like List but returns size and last-modified for each
// object, parsed from the listing response (no extra GETs). The flat List is
// kept for callers that only need names — changing its signature would touch
// every existing caller for no gain.
func (s *Store) ListWithMeta(ctx context.Context, slug string) ([]FileEntry, error) {
	prefix := slug + "/"
	infos, err := s.backend.list(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("failed to list objects in %s: %w", slug, err)
	}
	files := make([]FileEntry, 0, len(infos))
	for _, info := range infos {
		name := strings.TrimPrefix(info.key, prefix)
		if name == "" {
			continue
		}
		files = append(files, FileEntry{Path: name, Size: info.size, LastModified: info.lastModified})
	}
	return files, nil
}

func (s *Store) EnsureBucket(ctx context.Context) error {
	return s.backend.ensureBucket(ctx)
}

// Delete removes a single object at `{slug}/{path}`. Cache entry, if any, is
// evicted so subsequent Reads don't return a phantom object.
func (s *Store) Delete(ctx context.Context, slug, path string) error {
	key := slug + "/" + path
	err := s.backend.remove(ctx, key)
	if err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.Remove(key)
	}
	return nil
}

// Copy duplicates `{slug}/{srcPath}` to `{slug}/{dstPath}`. Preserves the
// source object's content-type and user metadata. Evicts the destination from
// the ARC cache so subsequent Reads pick up the new object.
func (s *Store) Copy(ctx context.Context, slug, srcPath, dstPath string) error {
	err := ValidateObjectPath(srcPath)
	if err != nil {
		return err
	}
	err = ValidateObjectPath(dstPath)
	if err != nil {
		return err
	}
	srcKey := slug + "/" + srcPath
	dstKey := slug + "/" + dstPath
	err = s.backend.copyObject(ctx, srcKey, dstKey)
	if err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.Remove(dstKey)
	}
	return nil
}

// UpdateMetadata replaces the user-defined metadata on `{slug}/{path}` without
// touching its bytes. Encoding mirrors Write (URL-escape values so unicode
// round-trips through ASCII-only metadata). Evicts the ARC cache so the next
// Read sees fresh metadata.
func (s *Store) UpdateMetadata(ctx context.Context, slug, path, contentType string, metadata map[string]string) error {
	err := ValidateObjectPath(path)
	if err != nil {
		return err
	}
	key := slug + "/" + path
	err = s.backend.replaceMeta(ctx, key, contentType, encodeMetadata(metadata))
	if err != nil {
		return err
	}
	if s.cache != nil {
		s.cache.Remove(key)
	}
	return nil
}

// Rename moves an object from srcPath to dstPath by Copy+Delete. Returns nil
// without doing any work when src == dst. If Copy succeeds but Delete fails
// the new object exists alongside the old one — surviving but inconsistent —
// and the delete error is returned so the caller can surface it.
func (s *Store) Rename(ctx context.Context, slug, srcPath, dstPath string) error {
	if srcPath == dstPath {
		return nil
	}
	err := s.Copy(ctx, slug, srcPath, dstPath)
	if err != nil {
		return err
	}
	err = s.Delete(ctx, slug, srcPath)
	if err != nil {
		slog.Warn("store.rename.delete_failed", "slug", slug, "src", srcPath, "dst", dstPath, "err", err)
		return err
	}
	return nil
}

// WriteRaw writes to an arbitrary bucket key, bypassing the slug-prefix
// convention. Used by snapshot infrastructure that stores archives under a
// reserved `_snapshots/` prefix outside any user slug. No metadata encoding;
// pass already-ASCII values.
func (s *Store) WriteRaw(ctx context.Context, key, content, contentType string, metadata map[string]string) error {
	if contentType == "" {
		contentType = DefaultContentType
	}
	_, err := s.backend.put(ctx, key, []byte(content), contentType, metadata)
	return err
}

// ReadRaw is the symmetric counterpart to WriteRaw: fetches an object by
// absolute bucket key. Returns an S3Object with empty Content for missing
// keys (no error). Bypasses the ARC cache and does not URL-decode metadata.
func (s *Store) ReadRaw(ctx context.Context, key string) (*S3Object, error) {
	raw, err := s.backend.get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("failed to get raw object %s: %w", key, err)
	}
	if raw == nil {
		return &S3Object{}, nil
	}
	return &S3Object{Content: string(raw.body), ETag: raw.etag, ContentType: raw.contentType, Metadata: raw.metadata}, nil
}

// DeleteRaw removes an object by absolute bucket key.
func (s *Store) DeleteRaw(ctx context.Context, key string) error {
	return s.backend.remove(ctx, key)
}

// ListPrefix returns absolute bucket keys under the given prefix. Used to
// enumerate snapshot archives at `_snapshots/{slug}/`.
func (s *Store) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	infos, err := s.backend.list(ctx, prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(infos))
	for _, info := range infos {
		keys = append(keys, info.key)
	}
	return keys, nil
}

// PrefixStats is the aggregate of SumBytesUnderPrefix: total bytes and object
// count beneath a bucket prefix.
type PrefixStats struct {
	TotalBytes  int64
	ObjectCount int
}

// SumBytesUnderPrefix aggregates total bytes + object count beneath an
// arbitrary bucket prefix in a single listing sweep — no per-object reads.
// Used by the system dashboard to break storage down by reserved area
// (_snapshots/, _edits/, _acme/, _state/) without round-tripping each archive.
// Returns a zero PrefixStats for a prefix with no objects so callers can render
// a zero row without special-casing missing folders.
func (s *Store) SumBytesUnderPrefix(ctx context.Context, prefix string) (PrefixStats, error) {
	infos, err := s.backend.list(ctx, prefix)
	if err != nil {
		return PrefixStats{}, err
	}
	var stats PrefixStats
	for _, info := range infos {
		stats.TotalBytes += info.size
		stats.ObjectCount++
	}
	return stats, nil
}

// ListApps returns the slugs of every site in the bucket. Top-level prefixes
// that start with "_" are reserved (e.g. _snapshots/) and excluded — slugs are
// restricted to [a-z0-9-] so this can never hide a real app.
func (s *Store) ListApps(ctx context.Context) ([]string, error) {
	tops, err := s.backend.listApps(ctx)
	if err != nil {
		return nil, err
	}
	apps := make([]string, 0, len(tops))
	for _, app := range tops {
		if app == "" || strings.HasPrefix(app, "_") {
			continue
		}
		apps = append(apps, app)
	}
	return apps, nil
}
