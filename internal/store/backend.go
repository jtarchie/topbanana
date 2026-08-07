package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// objectBackend is the low-level blob layer that Store sits on top of. The S3
// implementation (s3Backend) talks to a real bucket; the in-memory one
// (memoryBackend, see memory.go) backs fast deterministic tests. Everything
// that gives Store its behaviour — compression at rest, path validation,
// metadata URL-encoding, the ARC cache, the slug-prefix convention — lives in
// Store ABOVE this seam, so both backends exercise byte-identical higher-level
// semantics. A backend only moves opaque bytes + content-type + a verbatim
// metadata map around by key.
//
// Method names avoid the Go builtins delete/copy (revive's redefines-builtin-id
// rule), hence remove/copyObject.
type objectBackend interface {
	// put stores body at key, returning the new ETag. metadata is stored
	// verbatim — any encoding is Store's concern.
	put(ctx context.Context, key string, body []byte, contentType string, metadata map[string]string) (string, error)
	// putConditional stores body at key only if the precondition holds, and
	// returns ErrPrecondition when it doesn't. expectedETag == "" means "the
	// key must not exist yet" (If-None-Match: *); otherwise the stored object
	// must currently carry that exact ETag (If-Match). This is the primitive
	// that lets callers turn read-then-write into an atomic claim across
	// processes; without it, two instances racing on the same key both win.
	putConditional(ctx context.Context, key string, body []byte, contentType string, metadata map[string]string, expectedETag string) (string, error)
	// get fetches key. Returns (nil, nil) when the key does not exist, so a
	// clean miss is distinguishable from a backend fault (non-nil error).
	get(ctx context.Context, key string) (*rawObject, error)
	// list returns size + last-modified for every object whose key carries the
	// given prefix.
	list(ctx context.Context, prefix string) ([]objectInfo, error)
	// listApps returns the top-level "directory" names (S3 common prefixes under
	// delimiter "/") with the trailing slash stripped.
	listApps(ctx context.Context) ([]string, error)
	// remove deletes key (no error if it is already absent).
	remove(ctx context.Context, key string) error
	// copyObject duplicates srcKey to dstKey, preserving content-type and
	// metadata.
	copyObject(ctx context.Context, srcKey, dstKey string) error
	// replaceMeta rewrites key's metadata (and content-type when non-empty) in
	// place, leaving the bytes untouched.
	replaceMeta(ctx context.Context, key, contentType string, metadata map[string]string) error
	// ensureBucket creates the backing store if it does not yet exist.
	ensureBucket(ctx context.Context) error
}

// rawObject is one stored object as the backend sees it: opaque bytes plus the
// S3-level attributes. metadata is whatever was written (Store URL-encodes it
// on the way in and decodes on the way out).
type rawObject struct {
	body        []byte
	etag        string
	contentType string
	metadata    map[string]string
}

// objectInfo is the listing-level view of an object: its full key plus the two
// attributes ListObjectsV2 returns without a per-object GET.
type objectInfo struct {
	key          string
	size         int64
	lastModified time.Time
}

// s3Backend is the production objectBackend: a thin adapter over the AWS SDK's
// s3.Client scoped to one bucket. It carries no logic of its own — every method
// is a near-verbatim move of the call that used to live inline in Store.
type s3Backend struct {
	client *s3.Client
	bucket string
}

func (b *s3Backend) put(ctx context.Context, key string, body []byte, contentType string, metadata map[string]string) (string, error) {
	out, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(body)),
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	})
	if err != nil {
		return "", fmt.Errorf("failed to write object %s: %w", key, err)
	}
	if out != nil && out.ETag != nil {
		return *out.ETag, nil
	}
	return "", nil
}

func (b *s3Backend) putConditional(ctx context.Context, key string, body []byte, contentType string, metadata map[string]string, expectedETag string) (string, error) {
	in := &s3.PutObjectInput{
		Bucket:      aws.String(b.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(string(body)),
		ContentType: aws.String(contentType),
		Metadata:    metadata,
	}
	if expectedETag == "" {
		in.IfNoneMatch = aws.String("*")
	} else {
		in.IfMatch = aws.String(expectedETag)
	}
	out, err := b.client.PutObject(ctx, in)
	if err != nil {
		// An If-Match against a key that no longer exists comes back as 404
		// NoSuchKey from both S3 and Minio, not the 412 the HTTP spec would
		// suggest. For a caller it means the same thing the 412 does — the
		// object you based this write on is not there any more, so you lost —
		// and collapsing the two here is what lets callers treat
		// ErrPrecondition as the single "someone else won" signal instead of
		// re-deriving backend trivia.
		if isPreconditionFailed(err) || (expectedETag != "" && isMissingKey(err)) {
			return "", ErrPrecondition
		}
		return "", fmt.Errorf("failed to write object %s: %w", key, err)
	}
	if out != nil && out.ETag != nil {
		return *out.ETag, nil
	}
	return "", nil
}

// isMissingKey reports whether err is S3's "key does not exist". Checked via
// the typed error first, with a status/code fallback because Minio surfaces it
// as a plain 404 API error on the PutObject path rather than a NoSuchKey shape.
func isMissingKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode() == "NoSuchKey" || apiErr.ErrorCode() == "404"
	}
	return false
}

// isPreconditionFailed reports whether err is S3's 412 for a failed If-Match /
// If-None-Match. Mirrors internal/state's helper: the SDK surfaces this
// inconsistently across S3 and Minio, so the string fallbacks stay.
func isPreconditionFailed(err error) bool {
	if err == nil {
		return false
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		if code == "PreconditionFailed" || code == "412" {
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "PreconditionFailed") ||
		strings.Contains(msg, "precondition failed") ||
		strings.Contains(msg, "412")
}

func (b *s3Backend) get(ctx context.Context, key string) (*rawObject, error) {
	out, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return nil, nil //nolint:nilnil // a clean miss; Store maps it to an empty object.
		}
		return nil, fmt.Errorf("failed to get object %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read object %s: %w", key, err)
	}
	obj := &rawObject{body: body}
	if out.ETag != nil {
		obj.etag = *out.ETag
	}
	if out.ContentType != nil {
		obj.contentType = *out.ContentType
	}
	if len(out.Metadata) > 0 {
		obj.metadata = make(map[string]string, len(out.Metadata))
		for k, v := range out.Metadata {
			obj.metadata[strings.ToLower(k)] = v
		}
	}
	return obj, nil
}

// list walks every page. ListObjectsV2 returns at most 1000 keys per response,
// so the single call this used to make silently truncated: a site with more
// than 1000 objects listed 1000 of them and reported success, and the same cap
// applied to every prefix sweep layered on top (sessions, invites, snapshots).
// Nothing surfaced, because a short listing is indistinguishable from a small
// one.
func (b *s3Backend) list(ctx context.Context, prefix string) ([]objectInfo, error) {
	var infos []objectInfo
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(b.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list prefix %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			info := objectInfo{key: aws.ToString(obj.Key), size: aws.ToInt64(obj.Size)}
			if obj.LastModified != nil {
				info.lastModified = *obj.LastModified
			}
			infos = append(infos, info)
		}
	}
	return infos, nil
}

// listApps paginates for the same reason list does: the 1000-key response cap
// applies to CommonPrefixes too, so a bucket past that many sites would return
// a truncated roster to the site registry — and the registry is what backs
// routing and ownership, so a missing slug reads as a site that doesn't exist.
func (b *s3Backend) listApps(ctx context.Context) ([]string, error) {
	var apps []string
	pages := s3.NewListObjectsV2Paginator(b.client, &s3.ListObjectsV2Input{
		Bucket:    aws.String(b.bucket),
		Delimiter: aws.String("/"),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to list apps: %w", err)
		}
		for _, cp := range page.CommonPrefixes {
			apps = append(apps, strings.TrimSuffix(aws.ToString(cp.Prefix), "/"))
		}
	}
	return apps, nil
}

func (b *s3Backend) remove(ctx context.Context, key string) error {
	_, err := b.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(b.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("failed to delete object %s: %w", key, err)
	}
	return nil
}

func (b *s3Backend) copyObject(ctx context.Context, srcKey, dstKey string) error {
	_, err := b.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(b.bucket),
		Key:        aws.String(dstKey),
		CopySource: aws.String(b.bucket + "/" + srcKey),
	})
	if err != nil {
		return fmt.Errorf("failed to copy %s to %s: %w", srcKey, dstKey, err)
	}
	return nil
}

func (b *s3Backend) replaceMeta(ctx context.Context, key, contentType string, metadata map[string]string) error {
	input := &s3.CopyObjectInput{
		Bucket:            aws.String(b.bucket),
		Key:               aws.String(key),
		CopySource:        aws.String(b.bucket + "/" + key),
		Metadata:          metadata,
		MetadataDirective: types.MetadataDirectiveReplace,
	}
	if contentType != "" {
		input.ContentType = aws.String(contentType)
	}
	_, err := b.client.CopyObject(ctx, input)
	if err != nil {
		return fmt.Errorf("failed to update metadata on %s: %w", key, err)
	}
	return nil
}

func (b *s3Backend) ensureBucket(ctx context.Context) error {
	_, err := b.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(b.bucket)})
	if err == nil {
		return nil
	}
	_, err = b.client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(b.bucket)})
	if err != nil {
		var owned *types.BucketAlreadyOwnedByYou
		var exists *types.BucketAlreadyExists
		if errors.As(err, &owned) || errors.As(err, &exists) {
			return nil
		}
		return fmt.Errorf("failed to create bucket %s: %w", b.bucket, err)
	}
	return nil
}
