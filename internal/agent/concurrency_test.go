package agent

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/jtarchie/topbanana/internal/store"
)

// TestBuildState_WriteMuSerializesEdits pins the contract behind
// buildState.writeMu: when N goroutines do the same read-modify-write the
// mutating tools do (edit_file / replace_lines / insert_at_line / edit_function),
// every edit must land in the final content. No silent clobbers.
//
// We exercise the lock against a real S3-backed store so the test would catch
// a future regression where someone takes the lock out, or scopes it too
// narrowly (e.g. around Write only, missing the Read).
func TestBuildState_WriteMuSerializesEdits(t *testing.T) {
	s := agentMinioStore(t)
	if s == nil {
		t.Skip("AWS_ENDPOINT_URL / S3_BUCKET not set; skipping minio integration test")
	}
	ctx := context.Background()
	slug := "agent-writemu-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	path := "index.html"
	seed := "<!doctype html>\n<body>\n<!-- MARKERS -->\n</body>"
	t.Cleanup(func() {
		_ = s.Delete(ctx, slug, path)
	})

	err := s.Write(ctx, slug, path, seed, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	const editors = 6
	state := newBuildState()
	var wg sync.WaitGroup
	errs := make(chan error, editors)
	for i := range editors {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			marker := "<!-- edit-" + strconv.Itoa(i) + " -->\n"

			// Mirror the edit_file/replace_lines critical section: lock,
			// Read, mutate, Write, unlock (via defer).
			state.writeMu.Lock()
			defer state.writeMu.Unlock()

			obj, rerr := s.Read(ctx, slug, path)
			if rerr != nil {
				errs <- rerr
				return
			}
			updated := strings.Replace(obj.Content, "<!-- MARKERS -->", marker+"<!-- MARKERS -->", 1)
			werr := s.Write(ctx, slug, path, updated, "text/html; charset=utf-8", nil)
			if werr != nil {
				errs <- werr
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent edit: %v", e)
	}

	obj, err := s.Read(ctx, slug, path)
	if err != nil {
		t.Fatalf("read after edits: %v", err)
	}
	for i := range editors {
		want := "<!-- edit-" + strconv.Itoa(i) + " -->"
		if !strings.Contains(obj.Content, want) {
			t.Errorf("missing marker %q — an edit was silently dropped\nfinal content:\n%s", want, obj.Content)
		}
	}
}

func agentMinioStore(t *testing.T) *store.Store {
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
