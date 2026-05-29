package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"
)

// TestRenameRoundTrip exercises Copy + Rename against a live Minio bucket:
// the destination ends up with the source's content and the source is
// removed afterward. Covers the cache-eviction path by reading both keys
// before and after.
//
//nolint:cyclop // single end-to-end script keeps the related assertions next to each other.
func TestRenameRoundTrip(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	ctx := context.Background()
	slug := "rename-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})

	const content = "<h1>moved</h1>"
	err := s.Write(ctx, slug, "src.html", content, "text/html", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Copy preserves content; source survives.
	err = s.Copy(ctx, slug, "src.html", "copy.html")
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	if got, _ := s.Read(ctx, slug, "copy.html"); got == nil || got.Content != content {
		t.Errorf("copy.html content: got %+v want %q", got, content)
	}
	if got, _ := s.Read(ctx, slug, "src.html"); got == nil || got.Content != content {
		t.Errorf("src.html disappeared after copy")
	}

	// Rename moves the content and clears the source.
	err = s.Rename(ctx, slug, "src.html", "dst.html")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got, _ := s.Read(ctx, slug, "dst.html"); got == nil || got.Content != content {
		t.Errorf("dst.html content: got %+v want %q", got, content)
	}
	if got, _ := s.Read(ctx, slug, "src.html"); got != nil && got.Content != "" {
		t.Errorf("src.html survived rename: %q", got.Content)
	}

	// Same-path rename is a no-op (would otherwise destroy the file via
	// Copy-then-Delete).
	err = s.Rename(ctx, slug, "dst.html", "dst.html")
	if err != nil {
		t.Errorf("self-rename: %v", err)
	}
	if got, _ := s.Read(ctx, slug, "dst.html"); got == nil || got.Content != content {
		t.Errorf("dst.html lost after self-rename")
	}
}
