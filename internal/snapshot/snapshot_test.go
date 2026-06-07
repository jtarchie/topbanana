package snapshot_test

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"

	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
)

// minioStore connects to the dev minio (or any S3-compatible backend exposed
// via AWS_ENDPOINT_URL + S3_BUCKET) and returns a Store. Returns nil when the
// env vars aren't set so the caller can t.Skip().
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

func freshSlug(t *testing.T) string {
	t.Helper()
	return "snaptest-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func mustWrite(t *testing.T, ctx context.Context, s *store.Store, slug, path, content, ct string) {
	t.Helper()
	err := s.Write(ctx, slug, path, content, ct, nil)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(t *testing.T, ctx context.Context, s *store.Store, slug, path string) string {
	t.Helper()
	obj, err := s.Read(ctx, slug, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return obj.Content
}

func cleanupSlug(t *testing.T, ctx context.Context, s *store.Store, svc *snapshot.Service, slug string) {
	t.Helper()
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
		snaps, _ := svc.List(ctx, slug)
		for _, sn := range snaps {
			_ = svc.Delete(ctx, slug, sn.Key)
		}
	})
}

func TestSnapshotCreateAndList(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run snapshot integration tests")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	svc := snapshot.New(s, 0)
	cleanupSlug(t, ctx, s, svc, slug)

	mustWrite(t, ctx, s, slug, "index.html", "<h1>v1</h1>", "text/html")
	mustWrite(t, ctx, s, slug, "_state/data.json", `{"count":1}`, "application/json")

	snap1, err := svc.Create(ctx, slug, snapshot.ReasonBuild)
	if err != nil {
		t.Fatalf("create snap1: %v", err)
	}
	if snap1.FileCount != 2 {
		t.Fatalf("snap1 file count: got %d want 2", snap1.FileCount)
	}
	if snap1.SizeBytes == 0 {
		t.Fatalf("snap1 size: expected non-zero")
	}
	if !strings.Contains(snap1.Key, "_snapshots/"+slug+"/") {
		t.Fatalf("snap1 key: %q does not contain expected prefix", snap1.Key)
	}

	listed, err := svc.List(ctx, slug)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("list: got %d snapshots, want 1", len(listed))
	}
	if listed[0].Reason != snapshot.ReasonBuild {
		t.Fatalf("listed reason: %q want %q", listed[0].Reason, snapshot.ReasonBuild)
	}
}

func TestSnapshotRestore(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run snapshot integration tests")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	svc := snapshot.New(s, 0)
	cleanupSlug(t, ctx, s, svc, slug)

	// v1 of the site.
	mustWrite(t, ctx, s, slug, "index.html", "<h1>v1</h1>", "text/html")
	mustWrite(t, ctx, s, slug, "_state/data.json", `{"count":1}`, "application/json")
	snap1, err := svc.Create(ctx, slug, snapshot.ReasonBuild)
	if err != nil {
		t.Fatalf("create snap1: %v", err)
	}

	// Mutate to v2 — index changes, extra.html appears, KV count moves.
	mustWrite(t, ctx, s, slug, "index.html", "<h1>v2 different</h1>", "text/html")
	mustWrite(t, ctx, s, slug, "extra.html", "<p>added later</p>", "text/html")
	mustWrite(t, ctx, s, slug, "_state/data.json", `{"count":99}`, "application/json")

	err = svc.Restore(ctx, slug, snap1.Key)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}

	if got := mustRead(t, ctx, s, slug, "index.html"); got != "<h1>v1</h1>" {
		t.Fatalf("index content post-restore: got %q want %q", got, "<h1>v1</h1>")
	}
	if got := mustRead(t, ctx, s, slug, "_state/data.json"); got != `{"count":1}` {
		t.Fatalf("state post-restore: got %q want %q", got, `{"count":1}`)
	}
	if got := mustRead(t, ctx, s, slug, "extra.html"); got != "" {
		t.Fatalf("extra.html should have been wiped by restore, got %q", got)
	}

	// snap1 + auto pre-restore.
	listed, err := svc.List(ctx, slug)
	if err != nil {
		t.Fatalf("list post-restore: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("list post-restore: got %d, want 2 (snap1 + pre-restore)", len(listed))
	}
	foundPreRestore := false
	for _, sn := range listed {
		if sn.Reason == snapshot.ReasonPreRestore {
			foundPreRestore = true
		}
	}
	if !foundPreRestore {
		t.Fatalf("expected a pre-restore snapshot in list, got %+v", listed)
	}
}

// Archives written before the Bloomhollow rebrand carry PAX records with the
// BUILDABEAR.* prefix. The hand-crafted tarball below mimics one of those
// archives; Restore must read its per-file content type and metadata via the
// legacy fallback, otherwise old snapshots silently lose their type info.
func TestSnapshotRestoreReadsLegacyPAXHeaders(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run snapshot integration tests")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	svc := snapshot.New(s, 0)
	cleanupSlug(t, ctx, s, svc, slug)

	// Hand-craft a tar.zst archive with legacy BUILDABEAR.* PAX records.
	const indexBody = "<h1>legacy site</h1>"
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("init zstd: %v", err)
	}
	tw := tar.NewWriter(zw)
	hdr := &tar.Header{
		Name:    "index.html",
		Size:    int64(len(indexBody)),
		Mode:    0644,
		ModTime: time.Now().UTC(),
		PAXRecords: map[string]string{
			"BUILDABEAR.content-type":  "text/html; charset=utf-8",
			"BUILDABEAR.meta.x-legacy": "yes",
		},
	}
	err = tw.WriteHeader(hdr)
	if err != nil {
		t.Fatalf("write header: %v", err)
	}
	_, err = tw.Write([]byte(indexBody))
	if err != nil {
		t.Fatalf("write body: %v", err)
	}
	err = tw.Close()
	if err != nil {
		t.Fatalf("close tar: %v", err)
	}
	err = zw.Close()
	if err != nil {
		t.Fatalf("close zstd: %v", err)
	}

	// Park it under the snapshot prefix so Restore will accept the key.
	ts := time.Now().UTC().Format("20060102T150405Z")
	key := fmt.Sprintf("_snapshots/%s/%s-%s.tar.zst", slug, ts, snapshot.ReasonBuild)
	err = s.WriteRaw(ctx, key, buf.String(), "application/zstd", nil)
	if err != nil {
		t.Fatalf("write legacy archive: %v", err)
	}

	err = svc.Restore(ctx, slug, key)
	if err != nil {
		t.Fatalf("restore legacy archive: %v", err)
	}

	obj, err := s.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read restored index: %v", err)
	}
	if obj.Content != indexBody {
		t.Errorf("restored body: got %q want %q", obj.Content, indexBody)
	}
	if !strings.HasPrefix(obj.ContentType, "text/html") {
		t.Errorf("legacy PAX content-type lost: got %q", obj.ContentType)
	}
	if obj.Metadata["x-legacy"] != "yes" {
		t.Errorf("legacy PAX metadata lost: got %+v", obj.Metadata)
	}
}

func TestRetentionTrim(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run snapshot integration tests")
	}

	ctx := context.Background()
	slug := freshSlug(t)
	svc := snapshot.New(s, 2) // keep only 2

	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
		snaps, _ := svc.List(ctx, slug)
		for _, sn := range snaps {
			_ = svc.Delete(ctx, slug, sn.Key)
		}
	})

	err := s.Write(ctx, slug, "index.html", "<h1>seed</h1>", "text/html", nil)
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	for i := range 4 {
		_, err := svc.Create(ctx, slug, "build")
		if err != nil {
			t.Fatalf("create #%d: %v", i, err)
		}
		// Force distinct timestamps in the key (RFC3339 basic format has
		// 1-second resolution).
		time.Sleep(1100 * time.Millisecond)
	}

	listed, err := svc.List(ctx, slug)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("retention: got %d snapshots after 4 creates with keep=2, want 2", len(listed))
	}
}
