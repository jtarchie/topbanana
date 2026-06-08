package portable_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/klauspost/compress/zstd"

	"github.com/jtarchie/topbanana/internal/archive"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/portable"
	"github.com/jtarchie/topbanana/internal/store"
)

// minioStore mirrors the helper in snapshot_test so portable tests can run
// against the same dev minio set up by `task minio:ready`.
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

func freshSlug(t *testing.T, prefix string) string {
	t.Helper()
	return prefix + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cleanupSlug(t *testing.T, ctx context.Context, s *store.Store, slug string) {
	t.Helper()
	t.Cleanup(func() {
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})
}

// TestImportRejectsOversizedArchive proves the size check happens before any
// zstd decode — corrupt bytes that exceed the cap should error on size, not
// on decode.
func TestImportRejectsOversizedArchive(t *testing.T) {
	big := make([]byte, portable.MaxArchiveBytes+1)
	_, err := portable.Import(context.Background(), nil, "irrelevant", big)
	if !errors.Is(err, portable.ErrArchiveTooLarge) {
		t.Fatalf("got %v, want ErrArchiveTooLarge", err)
	}
}

func TestImportRejectsCorruptArchive(t *testing.T) {
	_, err := portable.Import(context.Background(), nil, "irrelevant", []byte("not a zstd stream"))
	if !errors.Is(err, portable.ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

// TestExportImportRoundTrip is the happy path: seed a slug, Export, Import
// to a fresh slug, confirm every non-reserved file came across and the
// manifest fields populate ImportResult.
//
//nolint:cyclop // single end-to-end script asserts many independent invariants on one round-trip.
func TestExportImportRoundTrip(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run portable integration tests")
	}

	ctx := context.Background()
	src := freshSlug(t, "porttest-src")
	dst := freshSlug(t, "porttest-dst")
	cleanupSlug(t, ctx, s, src)
	cleanupSlug(t, ctx, s, dst)

	// Seed the source with a representative mix: an HTML page, a CSS asset
	// with a non-default content type, a state file that must be stripped,
	// and a meta sidecar that must be stripped.
	mustWriteFile(t, ctx, s, src, "index.html", "<h1>hello</h1>", "text/html; charset=utf-8")
	mustWriteFile(t, ctx, s, src, "styles.css", "body{color:red}", "text/css")
	mustWriteFile(t, ctx, s, src, "_state/data.json", `{"secret":"do not export"}`, "application/json")

	meta := build.SiteMeta{
		Template:    "blank",
		Title:       "My Test Site",
		Description: "round-trip",
		OwnerID:     "alice@example.com",
		Created:     time.Now(),
	}
	writeMeta(t, ctx, s, src, meta)

	archive, err := portable.Export(ctx, s, src, meta)
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(archive) == 0 {
		t.Fatalf("export returned empty archive")
	}

	// Inspect the archive contents directly: meta sidecar and _state/ entries
	// must not be present; manifest must be present with the right fields.
	names, manifest := tarEntries(t, archive)
	if names[portable.ManifestPath] == nil {
		t.Fatalf("manifest entry missing from archive (entries: %v)", keys(names))
	}
	if names[build.MetaFile] != nil {
		t.Fatalf("meta sidecar leaked into archive")
	}
	if names[".buildabear.json"] != nil {
		t.Fatalf("legacy meta sidecar leaked into archive")
	}
	if names["_state/data.json"] != nil {
		t.Fatalf("_state/ leaked into archive")
	}
	if manifest.Template != "blank" || manifest.Title != "My Test Site" {
		t.Fatalf("manifest fields wrong: %+v", manifest)
	}
	if manifest.Version != portable.ManifestVersion {
		t.Fatalf("manifest version: got %d, want %d", manifest.Version, portable.ManifestVersion)
	}

	result, err := portable.Import(ctx, s, dst, archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.Template != "blank" || result.Title != "My Test Site" || result.Description != "round-trip" {
		t.Fatalf("import result didn't carry manifest: %+v", result)
	}
	if result.FileCount != 2 {
		t.Fatalf("import file count: got %d, want 2 (index.html + styles.css)", result.FileCount)
	}

	if got := readFile(t, ctx, s, dst, "index.html"); got != "<h1>hello</h1>" {
		t.Fatalf("index.html in dst: got %q", got)
	}
	if got := readFile(t, ctx, s, dst, "styles.css"); got != "body{color:red}" {
		t.Fatalf("styles.css in dst: got %q", got)
	}
	// _state/ must NOT have travelled.
	if got := readFile(t, ctx, s, dst, "_state/data.json"); got != "" {
		t.Fatalf("_state/data.json was imported, got %q", got)
	}
	// Meta sidecar must NOT have travelled either — the handler writes it
	// fresh on the destination, the package never plants it.
	if got := readFile(t, ctx, s, dst, build.MetaFile); got != "" {
		t.Fatalf("meta sidecar was imported (instance-specific OwnerID would leak): %q", got)
	}
}

// TestImportRequiresIndex builds an archive with one stray asset but no
// index.html and verifies Import refuses it.
func TestImportRequiresIndex(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run portable integration tests")
	}

	ctx := context.Background()
	dst := freshSlug(t, "porttest-noindex")
	cleanupSlug(t, ctx, s, dst)

	archive := buildHandCraftedArchive(t, []tarEntry{
		{name: "about.html", body: []byte("<p>orphan</p>"), contentType: "text/html"},
	})

	_, err := portable.Import(ctx, s, dst, archive)
	if !errors.Is(err, portable.ErrNoIndex) {
		t.Fatalf("got %v, want ErrNoIndex", err)
	}
}

// TestImportFiltersReservedPaths confirms that a hand-crafted archive that
// tries to plant a `.bloomhollow.json` or a `_state/` file is silently
// ignored — defense in depth against a crafted archive that an export-side
// filter wouldn't catch on this instance.
func TestImportFiltersReservedPaths(t *testing.T) {
	s := minioStore(t)
	if s == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run portable integration tests")
	}

	ctx := context.Background()
	dst := freshSlug(t, "porttest-defense")
	cleanupSlug(t, ctx, s, dst)

	hostileMeta, _ := json.Marshal(build.SiteMeta{
		OwnerID: "attacker@example.com",
	})
	archive := buildHandCraftedArchive(t, []tarEntry{
		{name: "index.html", body: []byte("<h1>legit</h1>"), contentType: "text/html"},
		{name: build.MetaFile, body: hostileMeta, contentType: "application/json"},
		{name: "_state/data.json", body: []byte(`{"x":1}`), contentType: "application/json"},
	})

	result, err := portable.Import(ctx, s, dst, archive)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if result.FileCount != 1 {
		t.Fatalf("file count: got %d, want 1 (only index.html should write)", result.FileCount)
	}
	if got := readFile(t, ctx, s, dst, build.MetaFile); got != "" {
		t.Fatalf("attacker meta was written: %q", got)
	}
	if got := readFile(t, ctx, s, dst, "_state/data.json"); got != "" {
		t.Fatalf("attacker state was written: %q", got)
	}
}

// --- helpers ---

type tarEntry struct {
	name        string
	body        []byte
	contentType string
}

func buildHandCraftedArchive(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{
			Name:    e.name,
			Size:    int64(len(e.body)),
			Mode:    0644,
			ModTime: time.Now(),
		}
		if e.contentType != "" {
			hdr.PAXRecords = map[string]string{archive.PAXContentTypeKey: e.contentType}
		}
		err := tw.WriteHeader(hdr)
		if err != nil {
			t.Fatalf("tar header %s: %v", e.name, err)
		}
		_, err = tw.Write(e.body)
		if err != nil {
			t.Fatalf("tar write %s: %v", e.name, err)
		}
	}
	err = tw.Close()
	if err != nil {
		t.Fatalf("close tar: %v", err)
	}
	err = zw.Close()
	if err != nil {
		t.Fatalf("close zstd: %v", err)
	}
	return buf.Bytes()
}

func tarEntries(t *testing.T, archive []byte) (map[string][]byte, portable.Manifest) {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("zstd open: %v", err)
	}
	defer zr.Close()
	out := map[string][]byte{}
	var manifest portable.Manifest
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		body, _ := readAll(t, tr)
		out[hdr.Name] = body
		if hdr.Name == portable.ManifestPath {
			_ = json.Unmarshal(body, &manifest)
		}
	}
	return out, manifest
}

func readAll(t *testing.T, tr *tar.Reader) ([]byte, error) {
	t.Helper()
	var buf bytes.Buffer
	_, err := buf.ReadFrom(tr)
	return buf.Bytes(), err
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func mustWriteFile(t *testing.T, ctx context.Context, s *store.Store, slug, path, content, ct string) {
	t.Helper()
	err := s.Write(ctx, slug, path, content, ct, nil)
	if err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, ctx context.Context, s *store.Store, slug, path string) string {
	t.Helper()
	obj, err := s.Read(ctx, slug, path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return obj.Content
}

// writeMeta seeds a `.bloomhollow.json` directly via the store so the test
// doesn't need a build.Service instance. The portable package reads meta
// via its caller, so this is enough to exercise the export filter.
func writeMeta(t *testing.T, ctx context.Context, s *store.Store, slug string, meta build.SiteMeta) {
	t.Helper()
	body, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		t.Fatalf("marshal meta: %v", err)
	}
	err = s.Write(ctx, slug, build.MetaFile, string(body), "application/json", nil)
	if err != nil {
		t.Fatalf("write meta: %v", err)
	}
}
