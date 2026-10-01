package store_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/internal/store"
)

func TestRetryThrottled_RetriesR2WriteThrottle(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`<Error><Code>ServiceUnavailable</Code><Message>Reduce your concurrent request rate for the same object.</Message></Error>`))
			return
		}
		w.Header().Set("ETag", `"abc"`)
	}))
	t.Cleanup(srv.Close)

	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)
	client := s3.New(s3.Options{
		Region:       "auto",
		BaseEndpoint: aws.String(srv.URL),
		UsePathStyle: true,
		HTTPClient:   &http.Client{Transport: transport},
		Credentials: aws.CredentialsProviderFunc(func(context.Context) (aws.Credentials, error) {
			return aws.Credentials{AccessKeyID: "k", SecretAccessKey: "s"}, nil
		}),
	}, store.RetryThrottled)

	_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String("b"),
		Key:    aws.String("k"),
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if got := hits.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 (one throttled, one retried)", got)
	}
}
