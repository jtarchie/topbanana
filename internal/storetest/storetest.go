// Package storetest provides the shared test helpers for constructing a
// store.Store and unique per-test slugs.
//
// By default New returns an in-memory store, so the whole storage layer — and
// everything layered on it (editrec, snapshot, portable, build, auth) — runs
// deterministically under `go test` with no infrastructure. When
// AWS_ENDPOINT_URL + S3_BUCKET are set it returns a real S3/Minio-backed store
// instead, so the same suites double as wire-fidelity conformance against a
// live bucket. This replaces the ~25-line minioStore bootstrap that used to be
// copy-pasted (and skipped without env) across a half-dozen packages.
package storetest

import (
	"context"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/internal/store"
)

// New returns a *store.Store for tests. It is in-memory unless AWS_ENDPOINT_URL
// + S3_BUCKET are both set, in which case it talks to that bucket (creating it
// if needed). cacheSize is passed straight through (use 0 to disable the read
// cache in tests that want every call to hit the backend).
func New(t *testing.T, cacheSize int) *store.Store {
	t.Helper()
	if !IsRemote() {
		s, err := store.NewInMemory(cacheSize)
		if err != nil {
			t.Fatalf("store.NewInMemory: %v", err)
		}
		return s
	}
	s, err := store.New(S3Client(t), os.Getenv("S3_BUCKET"), cacheSize)
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	err = s.EnsureBucket(context.Background())
	if err != nil {
		t.Fatalf("ensure bucket: %v", err)
	}
	return s
}

// S3Client builds an S3 client pointed at AWS_ENDPOINT_URL for tests, on an
// HTTP transport this package owns and tears down.
//
// The ownership is the point. The SDK's default client keeps connections in a
// pool that outlives the test, and every package here runs
// goleak.VerifyTestMain — so against a real bucket the pooled readLoop /
// writeLoop goroutines were reported as leaks and every S3-touching package
// failed. That made goleak's signal worthless in exactly the mode most likely
// to expose a genuine leak.
//
// Closing idle connections on cleanup fixes it without an ignore-list, which
// would have suppressed real net/http leaks in the SSE, sandbox, and LLM paths
// too. Keep-alives are disabled as well so nothing is pooled in the first
// place: the alternative is racing the connection teardown against goleak's
// check, and against a local bucket the extra handshakes cost nothing.
func S3Client(t *testing.T) *s3.Client {
	t.Helper()
	transport := &http.Transport{DisableKeepAlives: true}
	t.Cleanup(transport.CloseIdleConnections)

	cfg, err := config.LoadDefaultConfig(context.Background(),
		config.WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatalf("load aws config: %v", err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("AWS_ENDPOINT_URL"))
		o.UsePathStyle = true
	})
}

// IsRemote reports whether New returns an S3-backed store (both env vars set)
// rather than the in-memory default. Tests asserting genuinely S3-specific wire
// behaviour can t.Skip() when this is false.
func IsRemote() bool {
	return os.Getenv("AWS_ENDPOINT_URL") != "" && os.Getenv("S3_BUCKET") != ""
}

// slugSeq makes FreshSlug unique within a process; the pid segment makes it
// unique across runs against a shared bucket.
var slugSeq atomic.Uint64

// FreshSlug returns a valid, process-unique slug with the given prefix
// ([a-z0-9-], 3-30 chars). Keep prefix short so the result stays under 30
// characters.
func FreshSlug(t *testing.T, prefix string) string {
	t.Helper()
	pid := strconv.FormatInt(int64(os.Getpid()), 36)
	seq := strconv.FormatUint(slugSeq.Add(1), 36)
	return prefix + "-" + pid + seq
}
