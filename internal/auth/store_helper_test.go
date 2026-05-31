package auth

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/internal/store"
)

// minioStore connects to the dev minio (or any S3-compatible backend exposed
// via AWS_ENDPOINT_URL + S3_BUCKET) and returns a Store. Returns nil when
// either env var is unset so the caller can t.Skip() — every store-backed
// auth test follows the same skip convention used elsewhere in the repo.
func minioStore(t *testing.T) *store.Store {
	t.Helper()
	endpoint := os.Getenv("AWS_ENDPOINT_URL")
	bucket := os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		return nil
	}
	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	s, err := store.New(client, bucket, 0)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	err = s.EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return s
}

// freshSuffix is a per-test-run unique string so re-runs against the same
// bucket don't see stale data.
func freshSuffix() string {
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}
