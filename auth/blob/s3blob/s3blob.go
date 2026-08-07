// Package s3blob implements blob.Blobs over an S3-compatible object store:
// AWS S3, Cloudflare R2, MinIO, Tigris, anything speaking the same API.
//
// It is a separate package from blob so that importing the contract — or the
// in-memory implementation — does not drag the AWS SDK into a build that has
// no use for it.
//
// # Conditional writes
//
// The single-use guarantees in auth/oauth rest on compare-and-set, so the
// backing store must honour If-Match / If-None-Match on PutObject. AWS S3,
// MinIO, and Cloudflare R2 all do. Verify with blobtest.Run against your
// actual endpoint before trusting it: a store that ignores the precondition
// returns success to every racing writer, and nothing above this layer can
// tell.
//
// # R2
//
// R2 needs `region: "auto"` and its account-specific endpoint on the client;
// path-style addressing is not required. Construct the *s3.Client however your
// application already does and hand it to New.
package s3blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/jtarchie/topbanana/auth/blob"
)

// contentType is what every document written here carries. The auth domain
// stores only JSON, so the contract doesn't take one.
const contentType = "application/json"

// Store is a blob.Blobs backed by one S3 bucket.
type Store struct {
	client *s3.Client
	bucket string
}

// New returns a Store writing to bucket via client. The client carries the
// endpoint, credentials, and addressing style — this package makes no
// assumptions about which provider is on the other end.
func New(client *s3.Client, bucket string) *Store {
	return &Store{client: client, bucket: bucket}
}

func (s *Store) Get(ctx context.Context, key string) (blob.Object, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		if isMissingKey(err) {
			// A miss is an ordinary answer, not a fault. Reporting it as an
			// error is how a brief outage becomes "your credential is
			// invalid" to everything above.
			return blob.Object{}, nil
		}
		return blob.Object{}, fmt.Errorf("s3blob: get %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return blob.Object{}, fmt.Errorf("s3blob: read %s: %w", key, err)
	}
	return blob.Object{Content: string(body), ETag: aws.ToString(out.ETag)}, nil
}

func (s *Store) Put(ctx context.Context, key, content string) error {
	_, err := s.client.PutObject(ctx, s.putInput(key, content))
	if err != nil {
		return fmt.Errorf("s3blob: put %s: %w", key, err)
	}
	return nil
}

func (s *Store) PutIfMatch(ctx context.Context, key, content, etag string) error {
	if etag == "" {
		// An empty ETag would silently mean "no precondition" if passed
		// through, turning a compare-and-set into a blind overwrite.
		return fmt.Errorf("%w: PutIfMatch needs an ETag", blob.ErrPrecondition)
	}
	in := s.putInput(key, content)
	in.IfMatch = aws.String(etag)
	return s.conditional(ctx, key, in, true)
}

func (s *Store) PutIfAbsent(ctx context.Context, key, content string) error {
	in := s.putInput(key, content)
	in.IfNoneMatch = aws.String("*")
	return s.conditional(ctx, key, in, false)
}

func (s *Store) putInput(key, content string) *s3.PutObjectInput {
	return &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        strings.NewReader(content),
		ContentType: aws.String(contentType),
	}
}

// conditional runs a precondition-carrying PutObject and normalizes the
// "you lost" outcomes onto blob.ErrPrecondition. ifMatch says whether a
// missing key counts as a loss: S3 and R2 answer an If-Match against an absent
// object with 404 NoSuchKey rather than the 412 the HTTP spec implies, and to
// a caller that means the same thing — the object you based this write on is
// gone. If-None-Match has no such case, so a 404 there is a real fault.
func (s *Store) conditional(ctx context.Context, key string, in *s3.PutObjectInput, ifMatch bool) error {
	_, err := s.client.PutObject(ctx, in)
	if err == nil {
		return nil
	}
	if isPreconditionFailed(err) || (ifMatch && isMissingKey(err)) {
		return blob.ErrPrecondition
	}
	return fmt.Errorf("s3blob: conditional put %s: %w", key, err)
}

func (s *Store) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("s3blob: delete %s: %w", key, err)
	}
	return nil
}

// List returns every key under prefix, following continuation tokens.
//
// The pagination is not optional: ListObjectsV2 returns at most 1000 keys per
// response, so a single call silently truncates. The callers above this are a
// session sweep and an invite listing — both of which quietly stop working on
// the 1001st record, with no error to notice.
func (s *Store) List(ctx context.Context, prefix string) ([]string, error) {
	var keys []string
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("s3blob: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			keys = append(keys, aws.ToString(obj.Key))
		}
	}
	return keys, nil
}

// isPreconditionFailed reports whether err is the 412 for a failed If-Match /
// If-None-Match. The SDK surfaces this inconsistently across implementations,
// hence the string fallbacks.
func isPreconditionFailed(err error) bool {
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

// isMissingKey reports whether err is "that key does not exist". Checked via
// the typed error first, with a code fallback because some implementations
// surface it as a plain 404 on the PutObject path rather than a NoSuchKey
// shape.
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
