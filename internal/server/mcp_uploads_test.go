package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestUploadTicketCurl(t *testing.T) {
	t.Parallel()
	got := uploadTicketCurl("https://x.dev/upload/ticket/TOK", "my logo.png")
	if !strings.Contains(got, "--data-binary @LOCAL_FILE") {
		t.Errorf("recipe missing --data-binary placeholder: %q", got)
	}
	if !strings.Contains(got, "https://x.dev/upload/ticket/TOK?filename=my+logo.png") {
		t.Errorf("recipe should carry the url-escaped filename: %q", got)
	}
	// Empty filename falls back to a sensible default.
	if def := uploadTicketCurl("https://x.dev/u/TOK", ""); !strings.Contains(def, "filename=image.png") {
		t.Errorf("empty filename should default to image.png: %q", def)
	}
}

func TestAcceptedAssetTypes(t *testing.T) {
	t.Parallel()
	got := acceptedAssetTypes()
	if len(got) != len(allowedAssetTypes) {
		t.Fatalf("got %d types, want %d", len(got), len(allowedAssetTypes))
	}
	// Sorted + complete.
	want := strings.Join([]string{"image/gif", "image/jpeg", "image/png", "image/svg+xml", "image/webp"}, ",")
	if strings.Join(got, ",") != want {
		t.Errorf("acceptedAssetTypes = %v, want %s", got, want)
	}
}

func TestUploadTicketURL(t *testing.T) {
	t.Parallel()
	dev := &Server{domain: "localhost", port: "8080"}
	if got := dev.uploadTicketURL("TOK"); got != "http://localhost:8080/upload/ticket/TOK" {
		t.Errorf("dev upload URL = %q", got)
	}
	prod := &Server{domain: "apps.topbanana.dev"}
	if got := prod.uploadTicketURL("TOK"); got != "https://apps.topbanana.dev/upload/ticket/TOK" {
		t.Errorf("prod upload URL = %q", got)
	}
}

// TestUploadTicketHandlerBadToken drives the route end-to-end through echo so
// the :token path param is populated: an unparseable ticket must 401 before any
// store/auth work (so a nil store/auth Server is fine here).
func TestUploadTicketHandlerBadToken(t *testing.T) {
	t.Parallel()
	s := &Server{mcpSecret: "a-secret"}
	e := echo.New()
	e.POST("/upload/ticket/:token", s.uploadTicketHandler)

	req := httptest.NewRequest(http.MethodPost, "/upload/ticket/not-a-real-token", strings.NewReader("data"))
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("bad ticket = %d, want 401", rec.Code)
	}
}

// TestMCPSurfaceMentionsUploadTicket pins that the surface steers binary uploads
// to create_upload_ticket rather than base64-through-write_file.
func TestMCPSurfaceMentionsUploadTicket(t *testing.T) {
	t.Parallel()
	if !strings.Contains(mcpInstructions, "create_upload_ticket") {
		t.Error("instructions should mention create_upload_ticket for binary images")
	}
}
