package s3blob_test

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/auth/blob/blobtest"
	"github.com/jtarchie/topbanana/auth/blob/s3blob"
)

// This implementation only means anything if it is exercised against a real
// S3-compatible endpoint: the whole reason it exists separately from
// blob.Memory is the behaviour a memory map cannot reproduce — chiefly that
// an If-Match against a missing key comes back 404, not 412.
//
// Skips without AWS_ENDPOINT_URL + S3_BUCKET so the default `go test` stays
// infrastructure-free, exactly like the rest of the repo.
func TestS3Blob_Conformance(t *testing.T) {
	endpoint, bucket := os.Getenv("AWS_ENDPOINT_URL"), os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run the S3 conformance suite")
	}

	// The transport is owned here and torn down on cleanup: the SDK's default
	// client pools connections past the end of the test, which goleak reports
	// as a leak. Keep-alives off so nothing is pooled to race against.
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})

	blobtest.Run(t, func() blob.Blobs { return s3blob.New(client, bucket) })
}

// TestS3Blob_ListPaginates is the case a small bucket never reaches.
// ListObjectsV2 caps a response at 1000 keys, so an unpaginated List returns
// exactly 1000 and reports success — the session sweep above it would then
// stop collecting on the 1001st record with nothing to notice. Slow by
// necessity (it has to cross the boundary), so it skips under -short.
func TestS3Blob_ListPaginates(t *testing.T) {
	endpoint, bucket := os.Getenv("AWS_ENDPOINT_URL"), os.Getenv("S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run the S3 conformance suite")
	}
	if testing.Short() {
		t.Skip("writes 1001 objects; skipped under -short")
	}

	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
	store := s3blob.New(client, bucket)

	ctx := context.Background()
	prefix := "s3blob-paging-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/"
	const want = 1001
	for i := range want {
		err = store.Put(ctx, prefix+strconv.Itoa(i)+".json", "{}")
		if err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	t.Cleanup(func() {
		for i := range want {
			_ = store.Delete(context.Background(), prefix+strconv.Itoa(i)+".json")
		}
	})

	keys, err := store.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != want {
		t.Fatalf("List returned %d keys, want %d — a single ListObjectsV2 call stops at 1000", len(keys), want)
	}
}
