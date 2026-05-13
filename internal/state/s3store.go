package state

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// stateBlobKey is the per-site path inside the bucket. Lives under `_state/`
// so it's clearly out-of-band relative to user-visible pages and assets.
const stateBlobKey = "_state/data.json"

// stateContentType matches the actual payload. S3 echoes it back on GET; we
// don't currently inspect it, but it makes the bucket readable to a human
// using `mc cat` without guessing.
const stateContentType = "application/json"

// S3Store persists per-site KV state as one JSON blob per slug at
// `{slug}/_state/data.json`, using ETag-based optimistic concurrency. Every
// Load is one S3 GET, every Save is one S3 PUT; no caches, no goroutines, no
// background work.
type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) Load(ctx context.Context, slug string) (*Snapshot, error) {
	key := slug + "/" + stateBlobKey
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return NewSnapshot(), nil
		}
		return nil, fmt.Errorf("state load %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()

	body, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("state read %s: %w", key, err)
	}

	snap := NewSnapshot()
	if out.ETag != nil {
		snap.ETag = *out.ETag
	}
	if len(body) > 0 {
		err = json.Unmarshal(body, &snap.Data)
		if err != nil {
			return nil, fmt.Errorf("state decode %s: %w", key, err)
		}
		if snap.Data == nil {
			snap.Data = map[string]any{}
		}
	}
	return snap, nil
}

func (s *S3Store) Save(ctx context.Context, slug string, snap *Snapshot) error {
	body, err := json.Marshal(snap.Data)
	if err != nil {
		return fmt.Errorf("state marshal: %w", err)
	}
	key := slug + "/" + stateBlobKey

	in := &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(body),
		ContentType: aws.String(stateContentType),
	}
	// Two CAS modes: If-Match against the loaded ETag (overwrite-after-load),
	// or If-None-Match: * for first-time writes (no etag) to avoid clobbering
	// a concurrent creator.
	if snap.ETag != "" {
		in.IfMatch = aws.String(snap.ETag)
	} else {
		in.IfNoneMatch = aws.String("*")
	}

	out, err := s.client.PutObject(ctx, in)
	if err != nil {
		if isPreconditionFailed(err) {
			return ErrConflict
		}
		return fmt.Errorf("state save %s: %w", key, err)
	}
	if out != nil && out.ETag != nil {
		snap.ETag = *out.ETag
	}
	snap.Dirty = false
	return nil
}

// isPreconditionFailed checks if the error came from an If-Match / If-None-Match
// rejection. Different S3 implementations surface this differently — Minio
// uses code "PreconditionFailed", AWS uses "Precondition Failed" — so check
// both the structured smithy.APIError code and fall back to a string match
// on the error message.
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
