package store_test

import (
	"context"
	"strconv"
	"testing"
	"time"
)

func TestSumBytesUnderPrefix(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("AWS_ENDPOINT_URL / S3_BUCKET not set; skipping minio integration test")
	}
	ctx := context.Background()
	// Unique prefix per run so concurrent test runs (and leftover bucket
	// state from prior runs that didn't clean up) don't collide on counts.
	root := "_sumbytes-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/"

	// Two files under root/slugA (sizes 11 and 13) and one under root/slugB
	// (size 7) — verifies that a prefix-scoped sweep only counts its subtree.
	fixtures := []struct {
		key, body string
	}{
		{root + "slugA/index.html", "hello world"},   // 11 bytes
		{root + "slugA/about.html", "twenty bytes!"}, // 13
		{root + "slugB/index.html", "seven b"},       // 7
	}
	t.Cleanup(func() {
		for _, f := range fixtures {
			_ = s.DeleteRaw(ctx, f.key)
		}
	})
	for _, f := range fixtures {
		err := s.WriteRaw(ctx, f.key, f.body, "text/plain", nil)
		if err != nil {
			t.Fatalf("seed %s: %v", f.key, err)
		}
	}

	cases := []struct {
		name      string
		prefix    string
		wantBytes int64
		wantCount int
	}{
		{"empty bucket prefix not in use", "_sumbytes-test-nonexistent-zzz/", 0, 0},
		{"whole root catches everything", root, 11 + 13 + 7, 3},
		{"slugA subtree only", root + "slugA/", 11 + 13, 2},
		{"slugB subtree only", root + "slugB/", 7, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bytes, count, err := s.SumBytesUnderPrefix(ctx, tc.prefix)
			if err != nil {
				t.Fatalf("SumBytesUnderPrefix: %v", err)
			}
			if bytes != tc.wantBytes {
				t.Errorf("bytes: got %d want %d", bytes, tc.wantBytes)
			}
			if count != tc.wantCount {
				t.Errorf("count: got %d want %d", count, tc.wantCount)
			}
		})
	}
}
