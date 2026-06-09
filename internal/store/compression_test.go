package store_test

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// repeatableHTML returns a redundant HTML string of at least n bytes so the
// compression-shrink assertion doesn't depend on a magic input length.
func repeatableHTML(n int) string {
	const seed = "<p>repeatable HTML that compresses well, with attributes and tags.</p>"
	return strings.Repeat(seed, n/len(seed)+1)
}

// TestWriteCompressesText confirms HTML (and other text-y mimes) land on S3
// as zstd-compressed bytes — that's what shrinks the "Apps (live files)" row
// on /system's Storage breakdown. Asserted via the size ListWithMeta reports,
// which is exactly the figure summed into the breakdown total.
func TestWriteCompressesText(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	ctx := context.Background()
	slug := "compress-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})

	plaintext := repeatableHTML(8 * 1024)
	err := s.Write(ctx, slug, "index.html", plaintext, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.ListWithMeta(ctx, slug)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListWithMeta err=%v len=%d", err, len(entries))
	}
	stored := entries[0].Size
	if stored >= int64(len(plaintext))/2 {
		t.Errorf("compressed size suspiciously large: stored=%d plain=%d", stored, len(plaintext))
	}

	// Round-trip: Read must return the original plaintext, not the compressed
	// bytes. Every caller in the codebase (proxy, agent, MCP, lint) depends on
	// this contract.
	got, err := s.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Content != plaintext {
		t.Errorf("round-trip mismatch: stored %d bytes, read %d bytes", len(plaintext), len(got.Content))
	}
}

// TestWriteSkipsImage confirms a pre-compressed mime type is written without
// re-compression. Re-zstd'ing a PNG burns CPU and typically grows the payload
// slightly because the header isn't redundant.
func TestWriteSkipsImage(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	ctx := context.Background()
	slug := "compress-img-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})

	// PNG signature + IHDR-ish bytes. The exact bytes don't matter — what
	// matters is that Write preserves them verbatim (no compression frame
	// wrapped around them) so byte-perfect Read works.
	pngBytes := "\x89PNG\r\n\x1a\n" + strings.Repeat("\x00\x01\x02\x03", 4096)
	err := s.Write(ctx, slug, "assets/x.png", pngBytes, "image/png", nil)
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := s.ListWithMeta(ctx, slug)
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListWithMeta err=%v len=%d", err, len(entries))
	}
	if entries[0].Size != int64(len(pngBytes)) {
		t.Errorf("image was modified at rest: stored=%d original=%d", entries[0].Size, len(pngBytes))
	}

	got, err := s.Read(ctx, slug, "assets/x.png")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Content != pngBytes {
		t.Errorf("png round-trip mismatch (%d vs %d bytes)", len(got.Content), len(pngBytes))
	}
}

// TestReadDecodesLegacyUncompressed seeds a slug-prefixed key with raw HTML
// via WriteRaw (which doesn't compress), then asserts Read returns the
// plaintext unchanged. This is what makes the compression rollout free of a
// migration: every pre-compression object in S3 still loads through Read.
func TestReadDecodesLegacyUncompressed(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	ctx := context.Background()
	slug := "compress-legacy-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})

	plain := "<!doctype html><html><body><p>legacy uncompressed</p></body></html>"
	err := s.WriteRaw(ctx, slug+"/index.html", plain, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := s.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Content != plain {
		t.Errorf("legacy read mismatch: got %q want %q", got.Content, plain)
	}
}
