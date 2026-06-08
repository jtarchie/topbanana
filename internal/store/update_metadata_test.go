package store_test

import (
	"context"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"
)

// TestUpdateMetadata_PreservesBytes_ReplacesMetadata exercises UpdateMetadata
// against a live Minio bucket. The body must round-trip byte-for-byte and the
// new metadata must overwrite the old one (including unicode that has to
// URL-escape on the wire).
func TestUpdateMetadata_PreservesBytes_ReplacesMetadata(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	ctx := context.Background()
	slug := "umeta-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})

	const body = "\x89PNG\r\n\x1a\nfakeimagebytes"
	err := s.Write(ctx, slug, "assets/photo.png", body, "image/png", map[string]string{
		"alt":         "initial alt",
		"description": "initial description",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantHash := sha256.Sum256([]byte(body))

	err = s.UpdateMetadata(ctx, slug, "assets/photo.png", "image/png", map[string]string{
		"alt":         "Edited alt — em dash 🍌",
		"description": "Replaced description.",
	})
	if err != nil {
		t.Fatalf("UpdateMetadata: %v", err)
	}

	got, err := s.Read(ctx, slug, "assets/photo.png")
	if err != nil {
		t.Fatalf("Read after update: %v", err)
	}
	if got == nil {
		t.Fatal("Read returned nil")
	}
	gotHash := sha256.Sum256([]byte(got.Content))
	if gotHash != wantHash {
		t.Errorf("bytes changed after metadata update: got %x want %x", gotHash, wantHash)
	}
	if got.Metadata["alt"] != "Edited alt — em dash 🍌" {
		t.Errorf("alt: got %q want %q", got.Metadata["alt"], "Edited alt — em dash 🍌")
	}
	if got.Metadata["description"] != "Replaced description." {
		t.Errorf("description: got %q want %q", got.Metadata["description"], "Replaced description.")
	}
}

// TestUpdateMetadata_RejectsBadPath ensures the path validator runs before any
// S3 traffic, matching Read/Write.
func TestUpdateMetadata_RejectsBadPath(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run store integration tests")
	}
	err := s.UpdateMetadata(context.Background(), "anyslug", "../escape", "", nil)
	if err == nil {
		t.Fatal("expected error for traversal path")
	}
}
