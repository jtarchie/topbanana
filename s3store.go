package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"strings"

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

func NewStore(client *s3.Client, bucket string, cacheSize int) (*Store, error) {
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

const defaultContentType = "text/html; charset=utf-8"

func (s *Store) Write(ctx context.Context, slug, path, content, contentType string, metadata map[string]string) error {
	key := slug + "/" + path
	if contentType == "" {
		contentType = defaultContentType
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

	if s.cache != nil {
		etag := ""
		if out != nil && out.ETag != nil {
			etag = *out.ETag
		}
		s.cache.Add(key, &S3Object{
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

func (s *Store) Read(ctx context.Context, slug, path string) (*S3Object, error) {
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
		if app != "" {
			apps = append(apps, app)
		}
	}
	return apps, nil
}
