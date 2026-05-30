package server_test

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/jtarchie/bloomhollow/internal/build"
	"github.com/jtarchie/bloomhollow/internal/events"
	"github.com/jtarchie/bloomhollow/internal/portable"
	"github.com/jtarchie/bloomhollow/internal/snapshot"
)

// TestExportHandler_DownloadsArchive drives GET /export/:slug end-to-end and
// inspects the streamed archive to confirm it has the expected files,
// excludes the meta sidecar, and carries the right Content-Disposition.
func TestExportHandler_DownloadsArchive(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "xpsrc-" + freshSlug(t)[len("srvtest-"):]
	cleanupSlug(t, ctx, st, snapSvc, slug)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>exportme</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "styles.css", "body{}", "text/css")
	writeMeta(t, ctx, st, slug, build.SiteMeta{
		Template:    "blank",
		Title:       "Exportable",
		Description: "Hello",
		OwnerID:     testAdminUser,
	})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/export/"+slug, nil)
	if err != nil {
		t.Fatalf("new GET: %v", err)
	}
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET export: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != portable.ArchiveContentType {
		t.Fatalf("Content-Type: got %q want %q", ct, portable.ArchiveContentType)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") || !strings.Contains(cd, slug) {
		t.Fatalf("Content-Disposition: got %q, expected attachment with slug %q", cd, slug)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}

	names, manifest := readArchive(t, body)
	if names[portable.ManifestPath] == nil {
		t.Fatalf("manifest missing (entries: %v)", entryNames(names))
	}
	if manifest.Template != "blank" || manifest.Title != "Exportable" {
		t.Fatalf("manifest fields: %+v", manifest)
	}
	if names["index.html"] == nil {
		t.Fatalf("index.html missing from archive")
	}
	if names["styles.css"] == nil {
		t.Fatalf("styles.css missing from archive")
	}
	if names[build.MetaFile] != nil {
		t.Fatalf("meta sidecar leaked into archive — OwnerID would migrate to other instance")
	}
}

// TestImportHandler_CreatesNewSite is the happy-path E2E for upload: build an
// archive in-memory, POST it as multipart, follow the redirect, confirm the
// new slug is in /apps and has the expected file contents + meta.
func TestImportHandler_CreatesNewSite(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)

	archive := craftValidArchive(t, "imported", "<h1>fresh</h1>")

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	req := buildMultipartImport(t, httpSrv.URL+"/import", archive, "")
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("POST import: %v", err)
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/workspace/") {
		t.Fatalf("redirect: got %q, want /workspace/<slug>", loc)
	}
	dst := strings.TrimPrefix(loc, "/workspace/")
	if dst == "" {
		t.Fatalf("redirect slug empty: %q", loc)
	}
	t.Cleanup(func() { cleanupSlug(t, ctx, st, snapSvc, dst) })

	if got := mustRead(t, ctx, st, dst, "index.html"); got != "<h1>fresh</h1>" {
		t.Fatalf("index.html in dst: got %q", got)
	}

	buildSvc := build.New(st, nil, events.NewTracker(), snapSvc)
	meta := buildSvc.ReadMeta(ctx, dst)
	if meta.OwnerID != testAdminUser {
		t.Errorf("dst OwnerID: got %q want %q", meta.OwnerID, testAdminUser)
	}
	if meta.Template != "imported" {
		t.Errorf("dst Template carried from manifest: got %q want %q", meta.Template, "imported")
	}
	if meta.Created.IsZero() {
		t.Errorf("dst Created: zero, expected fresh timestamp")
	}
}

// TestImportHandler_RejectsArchiveWithoutIndex verifies the helpful error
// surfaces when the user uploads a tarball missing index.html.
func TestImportHandler_RejectsArchiveWithoutIndex(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// Hand-built archive with one stray asset and no index.html.
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	_ = tw.WriteHeader(&tar.Header{Name: "about.html", Size: 8, Mode: 0644, ModTime: time.Now()})
	_, _ = tw.Write([]byte("<p>orph</p>"[:8]))
	_ = tw.Close()
	_ = zw.Close()

	req := buildMultipartImport(t, httpSrv.URL+"/import", buf.Bytes(), "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "index.html") {
		t.Errorf("body should mention index.html: %q", string(body))
	}
}

// TestImportHandler_RejectsCorruptArchive verifies the friendly error on a
// non-tar.zst payload (e.g., someone uploading a zip by mistake).
func TestImportHandler_RejectsCorruptArchive(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	req := buildMultipartImport(t, httpSrv.URL+"/import", []byte("PK\x03\x04 fake zip"), "")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST import: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "tar.zst") {
		t.Errorf("body should mention tar.zst: %q", string(body))
	}
}

// TestExportHandler_RejectsNonOwner confirms requireSlugOwnership gates the
// route — an unauthenticated request gets bounced to /login by requireUser.
func TestExportHandler_RejectsNonOwner(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}

	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	slug := "xpdeny-" + freshSlug(t)[len("srvtest-"):]
	cleanupSlug(t, ctx, st, snapSvc, slug)
	mustWrite(t, ctx, st, slug, "index.html", "<h1>guarded</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	handler := buildServer(t, st, snapSvc)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	// No cookie attached.
	req, _ := http.NewRequest(http.MethodGet, httpSrv.URL+"/export/"+slug, nil)
	req.Host = "localhost"
	noRedirect := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatalf("GET export: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (login redirect)", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Errorf("redirect: got %q want /login", loc)
	}
}

// --- helpers ---

func craftValidArchive(t *testing.T, template, indexBody string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	tw := tar.NewWriter(zw)

	manifest := portable.Manifest{
		Version:    portable.ManifestVersion,
		Template:   template,
		ExportedAt: time.Now().UTC(),
	}
	mb, _ := json.Marshal(manifest)
	_ = tw.WriteHeader(&tar.Header{
		Name:    portable.ManifestPath,
		Size:    int64(len(mb)),
		Mode:    0644,
		ModTime: time.Now(),
		PAXRecords: map[string]string{
			snapshot.PAXContentTypeKey: "application/json",
		},
	})
	_, _ = tw.Write(mb)

	_ = tw.WriteHeader(&tar.Header{
		Name:    "index.html",
		Size:    int64(len(indexBody)),
		Mode:    0644,
		ModTime: time.Now(),
		PAXRecords: map[string]string{
			snapshot.PAXContentTypeKey: "text/html",
		},
	})
	_, _ = tw.Write([]byte(indexBody))

	_ = tw.Close()
	_ = zw.Close()
	return buf.Bytes()
}

func buildMultipartImport(t *testing.T, target string, archive []byte, slug string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if slug != "" {
		_ = mw.WriteField("slug", slug)
	}
	fw, err := mw.CreateFormFile("archive", "site.tar.zst")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	_, _ = fw.Write(archive)
	_ = mw.Close()

	req, err := http.NewRequest(http.MethodPost, target, &body)
	if err != nil {
		t.Fatalf("new POST: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	return req
}

func readArchive(t *testing.T, archive []byte) (map[string][]byte, portable.Manifest) {
	t.Helper()
	zr, err := zstd.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatalf("zstd open: %v", err)
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	out := map[string][]byte{}
	var manifest portable.Manifest
	for {
		hdr, err := tr.Next()
		if err != nil {
			break
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(tr)
		out[hdr.Name] = b.Bytes()
		if hdr.Name == portable.ManifestPath {
			_ = json.Unmarshal(b.Bytes(), &manifest)
		}
	}
	return out, manifest
}

func entryNames(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
