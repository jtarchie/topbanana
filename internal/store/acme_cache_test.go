package store_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"golang.org/x/crypto/acme/autocert"

	"github.com/jtarchie/bloomhollow/internal/store"
)

// minioStore mirrors the snapshot_test helper: returns nil when the env vars
// aren't set so callers can t.Skip().
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

func TestACMECache_RoundTrip(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("AWS_ENDPOINT_URL / S3_BUCKET not set; skipping minio integration test")
	}
	ctx := context.Background()
	// Unique prefix per run so parallel tests / leftover state don't collide.
	prefix := "_acme-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/"
	cache := store.NewACMECache(s, prefix)

	t.Cleanup(func() {
		_ = cache.Delete(ctx, "round-trip")
		_ = cache.Delete(ctx, "binary")
	})

	// Miss before any write.
	_, err := cache.Get(ctx, "round-trip")
	if !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}

	// Put then Get.
	want := []byte("account+key payload")
	err = cache.Put(ctx, "round-trip", want)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := cache.Get(ctx, "round-trip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}

	// Binary safety: cert/key blobs are DER-encoded, must survive
	// string<->[]byte conversion in the store.
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd, 0x7f, 0x80}
	err = cache.Put(ctx, "binary", binary)
	if err != nil {
		t.Fatalf("put binary: %v", err)
	}
	gotBin, err := cache.Get(ctx, "binary")
	if err != nil {
		t.Fatalf("get binary: %v", err)
	}
	if !bytes.Equal(gotBin, binary) {
		t.Fatalf("binary round-trip mismatch: got % x want % x", gotBin, binary)
	}

	// Delete then Get reports miss again.
	err = cache.Delete(ctx, "round-trip")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = cache.Get(ctx, "round-trip")
	if !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss after delete, got %v", err)
	}
}

func TestACMECache_PrefixNormalization(t *testing.T) {
	cache := store.NewACMECache(nil, "_acme")
	if cache.Prefix != "_acme/" {
		t.Errorf("expected trailing slash to be added, got %q", cache.Prefix)
	}
	cache = store.NewACMECache(nil, "_acme/")
	if cache.Prefix != "_acme/" {
		t.Errorf("expected idempotent normalization, got %q", cache.Prefix)
	}
}
