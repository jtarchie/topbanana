package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type Store struct {
	client *s3.Client
	bucket string
}

func NewStore(client *s3.Client, bucket string) *Store {
	return &Store{client: client, bucket: bucket}
}

func (s *Store) Write(ctx context.Context, slug, path, content string) error {
	key := slug + "/" + path
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader([]byte(content)),
		ContentType: aws.String("text/html; charset=utf-8"),
	})
	return err
}

func (s *Store) Read(ctx context.Context, slug, path string) (string, error) {
	key := slug + "/" + path
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		var nsk *types.NoSuchKey
		if errors.As(err, &nsk) {
			return "", nil
		}
		return "", err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	return string(b), err
}

func (s *Store) List(ctx context.Context, slug string) ([]string, error) {
	prefix := slug + "/"
	out, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	if err != nil {
		return nil, err
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
	content, err := s.Read(ctx, slug, path)
	return err == nil && content != ""
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
		return err
	}
	return nil
}
