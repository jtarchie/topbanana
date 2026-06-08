package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/hashicorp/golang-lru/arc/v2"
)

type Store struct {
	client *s3.Client
	bucket string
	cache  *arc.ARCCache[string, *S3Object]
}

func New(client *s3.Client, bucket string, cacheSize int) (*Store, error) {
	var cache *arc.ARCCache[string, *S3Object]
	if cacheSize > 0 {
		var err error
		cache, err = arc.NewARC[string, *S3Object](cacheSize)
		if err != nil {
			return nil, fmt.Errorf("failed to create ARC cache: %w", err)
		}
	}
	return &Store{client: client, bucket: bucket, cache: cache}, nil
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

func (s *Store) Write(ctx context.Context, slug, path, content, contentType string, metadata map[string]string) error {
	err := validateObjectPath(path)
	if err != nil {
		return err
	}
	key := slug + "/" + path
	if contentType == "" {
		contentType = DefaultContentType
	}

	// S3 user-defined metadata must be ASCII. URL-encode so unicode (e.g. an
	// alt-text with em-dashes or non-Latin characters) round-trips safely.
	var encoded map[string]string
	if len(metadata) > 0 {
		encoded = make(map[string]string, len(metadata))
		for k, v := range metadata {
			encoded[k] = url.QueryEscape(v)
		}
	}

	out, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(content),
		ContentType: aws.String(contentType),
		Metadata:    encoded,
	})
	if err != nil {
		return fmt.Errorf("failed to write object %s: %w", key, err)
	}

	// s itself is guaranteed non-nil here: callers (server, portable)
	// always pass a real *Store; calling Write on nil would have panicked
	// on the PutObject call above.
	if s.cache != nil { //nolint:nilaway // see comment.
		etag := ""
		if out != nil && out.ETag != nil {
			etag = *out.ETag
		}
		s.cache.Add(key, &S3Object{ //nolint:nilaway // see comment on outer if.
			Content:     content,
			ETag:        etag,
			ContentType: contentType,
			Metadata:    cloneMetadata(metadata),
		})
	}

	return nil
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

// validateObjectPath rejects relative-traversal segments, absolute paths, and
// Windows separators. The proxy handler already gates on this before reaching
// here, but every caller of Read/Write benefits from the same check —
// otherwise a future agent tool or handler could write objects at keys like
// `slug/../other/...` that escape the per-tenant prefix.
func validateObjectPath(path string) error {
	if strings.Contains(path, "..") || strings.HasPrefix(path, "/") || strings.Contains(path, `\`) {
		return fmt.Errorf("invalid object path %q", path)
	}
	return nil
}

func (s *Store) Read(ctx context.Context, slug, path string) (*S3Object, error) {
	err := validateObjectPath(path)
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

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			obj := &S3Object{}
			if s.cache != nil {
				s.cache.Add(key, obj)
			}
			return obj, nil
		}
		return nil, fmt.Errorf("failed to get object %s/%s: %w", slug, path, err)
	}
	defer func() {
		_ = out.Body.Close()
	}()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read object %s/%s: %w", slug, path, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
	}
	contentType := ""
	if out.ContentType != nil {
		contentType = *out.ContentType
	}

	// Decode user-defined metadata (URL-escaped on write).
	var metadata map[string]string
	if len(out.Metadata) > 0 {
		metadata = make(map[string]string, len(out.Metadata))
		for k, v := range out.Metadata {
			dec, decErr := url.QueryUnescape(v)
			if decErr != nil {
				dec = v
			}
			metadata[strings.ToLower(k)] = dec
		}
	}

	obj := &S3Object{
		Content:     string(b),
		ETag:        etag,
		ContentType: contentType,
		Metadata:    metadata,
	}

	if s.cache != nil {
		s.cache.Add(key, obj)
	}

	return obj, nil
}

func (s *Store) List(ctx context.Context, slug string) ([]string, error) {
	prefix := slug + "/"
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects in %s: %w", slug, err)
	}
	files := make([]string, 0, len(out.Contents))
	for _, obj := range out.Contents {
		name := strings.TrimPrefix(aws.ToString(obj.Key), prefix)
		if name != "" {
			files = append(files, name)
		}
	}
	return files, nil
}

// FileEntry is one row of ListWithMeta — the data the file explorer renders.
// LastModified comes straight from S3 and is already UTC.
type FileEntry struct {
	Path         string
	Size         int64
	LastModified time.Time
}

// ListWithMeta is like List but returns size and last-modified for each
// object, parsed from the ListObjectsV2 response (no extra GETs). The flat
// List is kept for callers that only need names — changing its signature would
// touch every existing caller for no gain.
func (s *Store) ListWithMeta(ctx context.Context, slug string) ([]FileEntry, error) {
	prefix := slug + "/"
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list objects in %s: %w", slug, err)
	}
	files := make([]FileEntry, 0, len(out.Contents))
	for _, obj := range out.Contents {
		name := strings.TrimPrefix(aws.ToString(obj.Key), prefix)
		if name == "" {
			continue
		}
		entry := FileEntry{Path: name, Size: aws.ToInt64(obj.Size)}
		if obj.LastModified != nil {
			entry.LastModified = *obj.LastModified
		}
		files = append(files, entry)
	}
	return files, nil
}

func (s *Store) EnsureBucket(ctx context.Context) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err == nil {
		return nil
	}
	_, err = s.client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(s.bucket),
	})
	if err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("failed to create bucket %s: %w", s.bucket, err)
	}
	return nil
}

// Delete removes a single object at `{slug}/{path}`. Cache entry, if any, is
// evicted so subsequent Reads don't return a phantom object.
func (s *Store) Delete(ctx context.Context, slug, path string) error {
	key := slug + "/" + path
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object %s: %w", key, err)
	}
	if s.cache != nil {
		s.cache.Remove(key)
	}
	return nil
}

// Copy duplicates `{slug}/{srcPath}` to `{slug}/{dstPath}` via s3.CopyObject.
// Preserves the source object's content-type and user metadata. Evicts the
// destination from the ARC cache so subsequent Reads pick up the new object.
func (s *Store) Copy(ctx context.Context, slug, srcPath, dstPath string) error {
	err := validateObjectPath(srcPath)
	if err != nil {
		return err
	}
	err = validateObjectPath(dstPath)
	if err != nil {
		return err
	}
	srcKey := slug + "/" + srcPath
	dstKey := slug + "/" + dstPath
	_, err = s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(s.bucket + "/" + srcKey),
	})
	if err != nil {
		return fmt.Errorf("failed to copy %s to %s: %w", srcKey, dstKey, err)
	}
	if s.cache != nil {
		s.cache.Remove(dstKey)
	}
	return nil
}

// UpdateMetadata replaces the user-defined metadata on `{slug}/{path}` without
// touching its bytes. S3 has no in-place metadata update, so this is a
// CopyObject onto the same key with MetadataDirective=REPLACE — the bytes are
// rewritten server-side and the new metadata wins. Encoding mirrors Write
// (URL-escape values so unicode round-trips through ASCII-only S3 headers).
// Evicts the ARC cache so the next Read sees fresh metadata.
func (s *Store) UpdateMetadata(ctx context.Context, slug, path, contentType string, metadata map[string]string) error {
	err := validateObjectPath(path)
	if err != nil {
		return err
	}
	key := slug + "/" + path

	var encoded map[string]string
	if len(metadata) > 0 {
		encoded = make(map[string]string, len(metadata))
		for k, v := range metadata {
			encoded[k] = url.QueryEscape(v)
		}
	}

	input := &s3.CopyObjectInput{
		Bucket:            aws.String(s.bucket),
		Key:               aws.String(key),
		CopySource:        aws.String(s.bucket + "/" + key),
		Metadata:          encoded,
		MetadataDirective: types.MetadataDirectiveReplace,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	_, err = s.client.CopyObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to update metadata on %s: %w", key, err)
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
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(content),
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	})
	if err != nil {
		return fmt.Errorf("failed to write raw object %s: %w", key, err)
	}
	return nil
}

// ReadRaw is the symmetric counterpart to WriteRaw: fetches an object by
// absolute bucket key. Returns an S3Object with empty Content for missing
// keys (no error). Bypasses the ARC cache.
func (s *Store) ReadRaw(ctx context.Context, key string) (*S3Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return &S3Object{}, nil
		}
		return nil, fmt.Errorf("failed to get raw object %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read raw object %s: %w", key, err)
	}
	etag := ""
	if out.ETag != nil {
		etag = *out.ETag
	}
	contentType := ""
	if out.ContentType != nil {
		contentType = *out.ContentType
	}
	var metadata map[string]string
	if len(out.Metadata) > 0 {
		metadata = make(map[string]string, len(out.Metadata))
		for k, v := range out.Metadata {
			metadata[strings.ToLower(k)] = v
		}
	}
	return &S3Object{Content: string(b), ETag: etag, ContentType: contentType, Metadata: metadata}, nil
}

// DeleteRaw removes an object by absolute bucket key.
func (s *Store) DeleteRaw(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete raw object %s: %w", key, err)
	}
	return nil
}

// ListPrefix returns absolute bucket keys under the given prefix. Used to
// enumerate snapshot archives at `_snapshots/{slug}/`.
func (s *Store) ListPrefix(ctx context.Context, prefix string) ([]string, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list prefix %s: %w", prefix, err)
	}
	keys := make([]string, 0, len(out.Contents))
	for _, obj := range out.Contents {
		keys = append(keys, aws.ToString(obj.Key))
	}
	return keys, nil
}

// SumBytesUnderPrefix aggregates total bytes + object count beneath an
// arbitrary bucket prefix in a single ListObjectsV2 sweep — no per-object
// reads. Used by the system dashboard to break storage down by reserved
// area (_snapshots/, _edits/, _acme/, _state/) without round-tripping each
// archive. Returns (0, 0, nil) for a prefix with no objects so callers can
// render a zero row without special-casing missing folders.
func (s *Store) SumBytesUnderPrefix(ctx context.Context, prefix string) (int64, int, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return 0, 0, fmt.Errorf("failed to sum prefix %s: %w", prefix, err)
	}
	var total int64
	count := 0
	for _, obj := range out.Contents {
		total += aws.ToInt64(obj.Size)
		count++
	}
	return total, count, nil
}

// ListApps returns the slugs of every site in the bucket. Top-level prefixes
// that start with "_" are reserved (e.g. _snapshots/) and excluded — slugs
// are restricted to [a-z0-9-] so this can never hide a real app.
func (s *Store) ListApps(ctx context.Context) ([]string, error) {
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket:    aws.String(s.bucket),
		Delimiter: aws.String("/"),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list apps: %w", err)
	}
	apps := make([]string, 0, len(out.CommonPrefixes))
	for _, cp := range out.CommonPrefixes {
		prefix := aws.ToString(cp.Prefix)
		app := strings.TrimSuffix(prefix, "/")
		if app == "" || strings.HasPrefix(app, "_") {
			continue
		}
		apps = append(apps, app)
	}
	return apps, nil
}
