package server

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"
)

func TestSanitizeAttachmentName(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"plain", "notes.md", "notes.md", false},
		{"spaces and ampersand", "Caroline & Paweł.html", "caroline-pawe.html", false},
		{"uppercase ext", "My Doc.MARKDOWN", "my-doc.markdown", false},
		{"path stripped", "../../etc/passwd.html", "passwd.html", false},
		{"htm kept", "page.htm", "page.htm", false},
		{"leading punctuation trimmed", "***hi***.md", "hi.md", false},
		{"non-ascii-only stem", "日本語.md", "asset.md", false},
		{"bad extension", "resume.pdf", "", true},
		{"no extension", "README", "", true},
		{"empty", "   ", "", true},
		{"dot", ".", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sanitizeAttachmentName(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("sanitizeAttachmentName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeAttachmentNameLengthCap(t *testing.T) {
	long := strings.Repeat("a", 200) + ".md"
	got, err := sanitizeAttachmentName(long)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) > 80 {
		t.Errorf("name length = %d, want <= 80", len(got))
	}
	if !strings.HasSuffix(got, ".md") {
		t.Errorf("extension dropped: %q", got)
	}
}

func TestSanitizeStem(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hello World", "hello-world"},
		{"a   b", "a-b"},
		{"a-b", "a-b"},
		{"a.b.c", "a-b-c"},
		{"under_score", "under_score"},
		{"!!!", "asset"},
		{"", "asset"},
		{"--trim--", "trim"},
	}
	for _, tc := range cases {
		if got := sanitizeStem(tc.in); got != tc.want {
			t.Errorf("sanitizeStem(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// buildAttachmentForm builds an echo.Context carrying a multipart body with one
// "attachment" file per name, so parseAttachments can be exercised directly.
func buildAttachmentForm(t *testing.T, files map[string]string, order []string) *echo.Context {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for _, name := range order {
		fw, err := mw.CreateFormFile("attachment", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		_, _ = fw.Write([]byte(files[name]))
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/build", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return echo.New().NewContext(req, httptest.NewRecorder())
}

func TestParseAttachmentsDedupeCollisions(t *testing.T) {
	// "a b.md" and "a-b.md" both sanitize to "a-b.md"; the literal "a-b-2.md"
	// must not collide with the generated suffix.
	order := []string{"a b.md", "a-b.md", "a-b-2.md"}
	files := map[string]string{
		"a b.md":   "# one",
		"a-b.md":   "# two",
		"a-b-2.md": "# three",
	}
	c := buildAttachmentForm(t, files, order)
	atts, err := parseAttachments(c)
	if err != nil {
		t.Fatalf("parseAttachments: %v", err)
	}
	if len(atts) != 3 {
		t.Fatalf("got %d attachments, want 3", len(atts))
	}
	seen := map[string]bool{}
	for _, a := range atts {
		if seen[a.Name] {
			t.Errorf("duplicate attachment name %q", a.Name)
		}
		seen[a.Name] = true
	}
	if len(seen) != 3 {
		t.Errorf("expected 3 distinct names, got %v", seen)
	}
}

func TestParseAttachmentsSanitizesMessyNames(t *testing.T) {
	order := []string{"Caroline & Paweł.html", "My Notes.md"}
	files := map[string]string{
		"Caroline & Paweł.html": "<p>hi</p>",
		"My Notes.md":           "# notes",
	}
	c := buildAttachmentForm(t, files, order)
	atts, err := parseAttachments(c)
	if err != nil {
		t.Fatalf("parseAttachments rejected messy names: %v", err)
	}
	if len(atts) != 2 {
		t.Fatalf("got %d attachments, want 2", len(atts))
	}
	want := map[string]bool{"caroline-pawe.html": true, "my-notes.md": true}
	for _, a := range atts {
		if !want[a.Name] {
			t.Errorf("unexpected sanitized name %q", a.Name)
		}
	}
}
