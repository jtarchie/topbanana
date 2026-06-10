package server_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/server"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// tinyPNG is a 1×1 PNG; http.DetectContentType sniffs the signature as
// image/png, which is all the upload handler needs to accept it.
const tinyPNGBase64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAAC0lEQVR4nGNgYGAAAAAEAAH2FzhVAAAAAElFTkSuQmCC"

func mustTinyPNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(tinyPNGBase64)
	if err != nil {
		t.Fatalf("decode png: %v", err)
	}
	return b
}

// multipartUpload builds a POST request carrying optional form fields and a set
// of named files under a single multipart field. Mirrors buildMultipartImport
// in portable_handlers_test.go but supports several files + extra fields.
func multipartUpload(t *testing.T, target, field string, files map[string][]byte, fields map[string]string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, v := range fields {
		_ = mw.WriteField(k, v)
	}
	for name, content := range files {
		fw, err := mw.CreateFormFile(field, name)
		if err != nil {
			t.Fatalf("create form file %q: %v", name, err)
		}
		_, _ = fw.Write(content)
	}
	err := mw.Close()
	if err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	return req
}

// TestUploadHandler_ImageNoPanic is the end-to-end regression for the
// production nil-pointer panic: an image upload against a factory-less
// build.Service (s.build can't resolve a vision model) must return 200 with
// no caption, never crash.
func TestUploadHandler_ImageNoPanic(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	req := multipartUpload(t, "/upload/"+slug, "file",
		map[string][]byte{"My Photo.png": mustTinyPNG(t)}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	var resp struct {
		Path        string `json:"path"`
		Alt         string `json:"alt"`
		ContentType string `json:"content_type"`
	}
	err := json.Unmarshal(rec.Body.Bytes(), &resp)
	if err != nil {
		t.Fatalf("decode response: %v; body=%q", err, rec.Body.String())
	}
	if resp.ContentType != "image/png" {
		t.Errorf("content_type: got %q want image/png", resp.ContentType)
	}
	if resp.Alt != "" {
		t.Errorf("alt: got %q, want empty (no vision model configured)", resp.Alt)
	}
	// Sanitized filename: "My Photo.png" -> "my-photo.png" under assets/.
	if resp.Path != "assets/my-photo.png" {
		t.Errorf("path: got %q want assets/my-photo.png", resp.Path)
	}
	if got := mustRead(t, ctx, st, slug, resp.Path); got == "" {
		t.Errorf("uploaded asset not stored at %q", resp.Path)
	}
}

// TestUploadHandler_RejectsNonImage confirms a non-image body is rejected with
// 415 (and that the rejection, too, doesn't panic on the caption path).
func TestUploadHandler_RejectsNonImage(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	req := multipartUpload(t, "/upload/"+slug, "file",
		map[string][]byte{"notes.txt": []byte("just some plain text, not an image")}, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status: got %d want 415; body=%q", rec.Code, rec.Body.String())
	}
}

// TestEditSubmit_BadAttachmentShowsDetail drives the 4xx-detail path end to
// end: a browser form POST (Accept: text/html) with an unsupported attachment
// extension must render the specific reason on the error page, not just the
// generic tagline.
func TestEditSubmit_BadAttachmentShowsDetail(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	handler := buildServer(t, st, snapSvc)

	req := multipartUpload(t, "/edit/"+slug, "attachment",
		map[string][]byte{"resume.pdf": []byte("%PDF-1.4 not allowed")},
		map[string]string{"prompt": "make the heading bigger"})
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "must end in") {
		t.Errorf("error page should show the specific reason; body=%q", body)
	}
}

// TestEditSubmit_AcceptsMessyAttachment confirms an HTML reference doc with a
// messy name (spaces + non-ASCII) is accepted rather than 400'd: the edit
// starts (303 to the workspace) instead of being rejected at parse time.
func TestEditSubmit_AcceptsMessyAttachment(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	// Use a real stub runner so the async build the handler kicks off can
	// complete cleanly rather than tripping over a nil-LLM agent runner.
	handler := buildServerWithRunner(t, st, snapSvc, &stubRunner{title: "T", desc: "D"})
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	req := multipartUpload(t, "/edit/"+slug, "attachment",
		map[string][]byte{
			"Caroline & Paweł.html": []byte("<p>reference</p>"),
			"My Notes.md":           []byte("# notes"),
		},
		map[string]string{"prompt": "use these references"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d want 303 (accepted); body=%q", rec.Code, rec.Body.String())
	}
}

// TestManagePage_ShowsCNAMETarget confirms the manage page renders the
// configured custom-domain CNAME target in the copy-paste DNS instructions.
func TestManagePage_ShowsCNAMETarget(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	snapSvc := snapshot.New(st, 0)
	cleanupSlug(t, ctx, st, snapSvc, slug)
	const target = "edge.example.net"
	handler := buildServerWithRunnerAndInfo(t, st, snapSvc, &stubRunner{},
		server.SystemInfo{CustomDomainCNAME: target})
	mustWrite(t, ctx, st, slug, "index.html", "<h1>hi</h1>", "text/html")
	writeMeta(t, ctx, st, slug, build.SiteMeta{Template: "blank", OwnerID: testAdminUser})

	req := httptest.NewRequest(http.MethodGet, "/manage/"+slug, nil)
	req.Host = "localhost"
	req.AddCookie(testSessionCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, target) {
		t.Errorf("manage page should show CNAME target %q", target)
	}
	if !strings.Contains(body, "CNAME") || !strings.Contains(body, "js-copy") {
		t.Errorf("manage page missing CNAME record block / copy button")
	}
}
