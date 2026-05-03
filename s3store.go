package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	Content string
	ETag    string
}

func (s *Store) Write(ctx context.Context, slug, path, content string) error {
	key := slug + "/" + path
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader([]byte(content)),
		ContentType: aws.String("text/html; charset=utf-8"),
	})
	if err != nil {
		return fmt.Errorf("failed to write object %s: %w", key, err)
	}

	// Update cache with the new content
	if s.cache != nil {
		s.cache.Add(key, &S3Object{
			Content: content,
			ETag:    "", // ETag will be updated on next read if needed
		})
	}

	return nil
}

func (s *Store) Read(ctx context.Context, slug, path string) (*S3Object, error) {
	key := slug + "/" + path

	// Check cache first
	if s.cache != nil {
		if cached, ok := s.cache.Get(key); ok {
			return cached, nil
		}
	}

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return &S3Object{}, nil
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

	obj := &S3Object{
		Content: string(b),
		ETag:    etag,
	}

	// Store in cache for future reads (only if content is not empty)
	if s.cache != nil && obj.Content != "" {
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

func (s *Store) Exists(ctx context.Context, slug, path string) bool {
	obj, err := s.Read(ctx, slug, path)
	return err == nil && obj.Content != ""
}

// InvalidateCacheEntry removes a specific key from the cache
func (s *Store) InvalidateCacheEntry(slug, path string) {
	if s.cache != nil {
		key := slug + "/" + path
		s.cache.Remove(key)
	}
}

// InvalidateSlugCache removes all cache entries for a specific slug
func (s *Store) InvalidateSlugCache(slug string) {
	if s.cache == nil {
		return
	}
	// Since ARC doesn't provide iteration, we'll need to track keys
	// For now, this is a placeholder for future enhancement
	// In a production system, you might track slug->keys mappings
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
