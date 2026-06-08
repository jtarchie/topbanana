package server_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/snapshot"
)

// TestAssetsListHandler_FiltersToAssetsPrefix seeds a mix of pages and image
// assets, then asserts GET /assets/:slug returns only the assets/ entries
// with their alt + description metadata round-tripping through the JSON
// response.
func TestAssetsListHandler_FiltersToAssetsPrefix(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	mustWrite(t, ctx, st, slug, "index.html", "<h1>hi</h1>", "text/html")
	mustWrite(t, ctx, st, slug, "about.html", "<h1>about</h1>", "text/html")
	err := st.Write(ctx, slug, "assets/photo.png", "\x89PNGfake", "image/png", map[string]string{
		"alt":         "a photo",
		"description": "captioned description",
	})
	if err != nil {
		t.Fatalf("seed asset: %v", err)
	}
	err = st.Write(ctx, slug, "assets/icon.svg", "<svg/>", "image/svg+xml", nil)
	if err != nil {
		t.Fatalf("seed asset 2: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/assets/"+slug, nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var got []struct {
		Path        string `json:"path"`
		URL         string `json:"url"`
		Alt         string `json:"alt"`
		Description string `json:"description"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	}
	err = json.Unmarshal(rec.Body.Bytes(), &got)
	if err != nil {
		t.Fatalf("decode: %v; body=%q", err, rec.Body.String())
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 assets, got %d: %+v", len(got), got)
	}
	byPath := map[string]int{}
	for i, r := range got {
		byPath[r.Path] = i
	}
	idx, ok := byPath["assets/photo.png"]
	if !ok {
		t.Fatalf("photo.png missing from response: %+v", got)
	}
	if got[idx].Alt != "a photo" || got[idx].Description != "captioned description" {
		t.Errorf("photo.png metadata: got alt=%q desc=%q", got[idx].Alt, got[idx].Description)
	}
	if got[idx].ContentType != "image/png" {
		t.Errorf("photo.png content_type: got %q want image/png", got[idx].ContentType)
	}
	if !strings.HasPrefix(got[idx].URL, "http://"+slug+".localhost") {
		t.Errorf("photo.png URL: got %q, want subdomain URL", got[idx].URL)
	}
	if _, ok := byPath["assets/icon.svg"]; !ok {
		t.Errorf("icon.svg missing from response: %+v", got)
	}
}

// TestAssetsListHandler_RejectsUnauth confirms GET /assets/:slug requires a
// session, matching the rest of the admin surface.
func TestAssetsListHandler_RejectsUnauth(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)

	req := httptest.NewRequest(http.MethodGet, "/assets/anyslug", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("unauth status: got %d want 303 (redirect to /login)", rec.Code)
	}
}

// TestAssetMetadataPatchHandler_ReplacesMetadata seeds an asset with one set
// of metadata, PATCHes it with a different set, and confirms the new values
// stick while the bytes stay identical.
func TestAssetMetadataPatchHandler_ReplacesMetadata(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	const body = "\x89PNGfakecontent\x00\x01\x02"
	err := st.Write(ctx, slug, "assets/banner.png", body, "image/png", map[string]string{
		"alt":         "old alt",
		"description": "old description",
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	wantHash := sha256.Sum256([]byte(body))

	patch := `{"alt":"new alt","description":"new description"}`
	req := httptest.NewRequest(http.MethodPatch, "/assets/"+slug+"/assets/banner.png", strings.NewReader(patch))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}

	obj, err := st.Read(ctx, slug, "assets/banner.png")
	if err != nil {
		t.Fatalf("read after patch: %v", err)
	}
	gotHash := sha256.Sum256([]byte(obj.Content))
	if gotHash != wantHash {
		t.Errorf("bytes changed after metadata patch: got %x want %x", gotHash, wantHash)
	}
	if obj.Metadata["alt"] != "new alt" || obj.Metadata["description"] != "new description" {
		t.Errorf("metadata not replaced: got alt=%q desc=%q", obj.Metadata["alt"], obj.Metadata["description"])
	}
}

// TestAssetMetadataPatchHandler_AltCapped enforces the 125-char cap matches
// the vision-captioner's so the rendered alt-text behavior stays consistent
// whether the caption came from the model or the user.
func TestAssetMetadataPatchHandler_AltCapped(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	err := st.Write(ctx, slug, "assets/x.png", "bytes", "image/png", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	huge := strings.Repeat("a", 200)
	patch := `{"alt":"` + huge + `","description":""}`
	req := httptest.NewRequest(http.MethodPatch, "/assets/"+slug+"/assets/x.png", strings.NewReader(patch))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	obj, _ := st.Read(ctx, slug, "assets/x.png")
	if len(obj.Metadata["alt"]) != 125 {
		t.Errorf("alt length: got %d want 125 (capped)", len(obj.Metadata["alt"]))
	}
}

// TestAssetMetadataPatchHandler_404OnMissing confirms a PATCH for an unknown
// asset returns 404 instead of silently materializing a zero-byte object via
// CopyObject — that would leak the metadata-replace primitive into a create
// path.
func TestAssetMetadataPatchHandler_404OnMissing(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	patch := `{"alt":"x","description":"y"}`
	req := httptest.NewRequest(http.MethodPatch, "/assets/"+slug+"/assets/does-not-exist.png", strings.NewReader(patch))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%q", rec.Code, rec.Body.String())
	}
}

// TestAssetDeleteHandler_RemovesObject seeds an asset, DELETEs it, and
// confirms the object is gone from the bucket.
func TestAssetDeleteHandler_RemovesObject(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	err := st.Write(ctx, slug, "assets/doomed.png", "bytes", "image/png", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	req := httptest.NewRequest(http.MethodDelete, "/assets/"+slug+"/assets/doomed.png", nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	obj, err := st.Read(ctx, slug, "assets/doomed.png")
	if err != nil {
		t.Fatalf("read after delete: %v", err)
	}
	if obj != nil && obj.Content != "" {
		t.Errorf("asset survived delete: %q", obj.Content)
	}
}

// TestAssetDeleteHandler_404OnMissing matches the PATCH 404 semantics: a
// DELETE for a non-existent asset must 404 rather than report success.
// Returning 200 here would let a typo (or a stale drawer cache) read as a
// successful delete, hiding the real state.
func TestAssetDeleteHandler_404OnMissing(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	req := httptest.NewRequest(http.MethodDelete, "/assets/"+slug+"/assets/never-was.png", nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404; body=%q", rec.Code, rec.Body.String())
	}
}

// TestAssetDeleteHandler_RejectsTraversal confirms `..` segments are blocked
// before any S3 traffic, matching the PATCH handler.
func TestAssetDeleteHandler_RejectsTraversal(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	slug := freshSlug(t)
	ctx := context.Background()
	cleanupSlug(t, ctx, st, snapSvc, slug)

	req := httptest.NewRequest(http.MethodDelete, "/assets/"+slug+"/assets/../../etc/passwd", nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%q", rec.Code, rec.Body.String())
	}
}

// TestAssetMetadataPatchHandler_RejectsTraversal ensures path validation runs
// even though the wildcard route accepts arbitrary slashes — a `..` segment
// must 400 instead of falling through to the store.
func TestAssetMetadataPatchHandler_RejectsTraversal(t *testing.T) {
	st := minioStore(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run server integration tests")
	}
	snapSvc := snapshot.New(st, 0)
	handler := buildServer(t, st, snapSvc)
	slug := freshSlug(t)
	ctx := context.Background()
	cleanupSlug(t, ctx, st, snapSvc, slug)

	patch := `{"alt":"x","description":"y"}`
	req := httptest.NewRequest(http.MethodPatch, "/assets/"+slug+"/assets/../../etc/passwd", strings.NewReader(patch))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%q", rec.Code, rec.Body.String())
	}
}
