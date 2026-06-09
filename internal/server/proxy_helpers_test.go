package server

import (
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/store"
)

func TestResolveContentType(t *testing.T) {
	t.Parallel()

	png := mime.TypeByExtension(".png")

	cases := []struct {
		name   string
		stored string
		file   string
		want   string
	}{
		{"stored non-default wins", "application/pdf", "weird.xyz", "application/pdf"},
		{"legacy default falls back to extension", store.DefaultContentType, "logo.png", png},
		{"empty stored falls back to extension", "", "logo.png", png},
		{"no extension defaults", "", "noext", store.DefaultContentType},
		{"unknown extension with empty stored defaults", "", "file.unknownext", store.DefaultContentType},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if tc.want == "" {
				t.Skip("platform mime table has no mapping for this extension")
			}
			got := resolveContentType(tc.stored, tc.file)
			if got != tc.want {
				t.Errorf("resolveContentType(%q, %q) = %q, want %q", tc.stored, tc.file, got, tc.want)
			}
		})
	}
}

func TestParseLastEventID(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in   string
		want int
	}{
		{"", -1},
		{"0", 0},
		{"5", 5},
		{"-3", -3},
		{"abc", -1},
		{"12x", -1},
		{"  7", -1}, // strconv.Atoi rejects surrounding space — a malformed header is a full replay
	}
	for _, tc := range cases {
		got := parseLastEventID(tc.in)
		if got != tc.want {
			t.Errorf("parseLastEventID(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSafeAssetName_ExactCases(t *testing.T) {
	t.Parallel()

	cases := []struct {
		original string
		ext      string
		want     string
	}{
		{"photo.jpg", ".png", "photo.png"}, // forces the sniffed extension
		{"", ".png", "asset.png"},          // empty/garbage stem becomes "asset"
	}
	for _, tc := range cases {
		got, err := safeAssetName(tc.original, tc.ext)
		if err != nil {
			t.Fatalf("safeAssetName(%q, %q): %v", tc.original, tc.ext, err)
		}
		if got != tc.want {
			t.Errorf("safeAssetName(%q, %q) = %q, want %q", tc.original, tc.ext, got, tc.want)
		}
	}
}

func TestSafeAssetName_SanitizesAndCaps(t *testing.T) {
	t.Parallel()

	got, err := safeAssetName("My Photo!.JPG", ".jpg")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, ".jpg") || !assetStemIsSafe(strings.TrimSuffix(got, ".jpg")) {
		t.Errorf("safeAssetName(%q) = %q, want a sanitized .jpg name", "My Photo!.JPG", got)
	}

	long, err := safeAssetName(strings.Repeat("a", 200)+".txt", ".txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(long) > 64 || !strings.HasSuffix(long, ".txt") {
		t.Errorf("safeAssetName(long) = %q (len %d), want a capped .txt name", long, len(long))
	}
}

func assetStemIsSafe(stem string) bool {
	for _, r := range stem {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return stem != ""
}

// ctxWithForm builds an echo.Context carrying a urlencoded form, for testing the
// pure context helpers without standing up a server.
func ctxWithForm(form url.Values) *echo.Context {
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	return echo.New().NewContext(req, httptest.NewRecorder())
}

func TestFileOpsNextURL_OpenRedirectGuard(t *testing.T) {
	t.Parallel()

	const slug = "myslug"
	fallbackPrefix := "/files/" + slug + "?flash="

	// next values that must be REJECTED (bounce back to the files-list fallback).
	rejected := []string{
		"",                    // no next
		"//evil.com",          // scheme-relative
		`/\evil.com`,          // backslash trick (doesn't match an allowed prefix)
		"https://evil.com",    // absolute off-site
		"/evil",               // not in allowlist
		"/files/otherslug",    // another tenant's slug
		"/files/myslugevil",   // prefix-extension trick
		"javascript:alert(1)", // not a path
	}
	for _, next := range rejected {
		c := ctxWithForm(url.Values{"next": {next}})
		got := fileOpsNextURL(c, slug, "done")
		if !strings.HasPrefix(got, fallbackPrefix) {
			t.Errorf("fileOpsNextURL(next=%q) = %q, want the safe fallback", next, got)
		}
	}

	// next values that must be ACCEPTED (an in-app, same-slug destination).
	accepted := []string{
		"/files/" + slug,
		"/files/" + slug + "/sub",
		"/workspace/" + slug + "?page=index.html",
		"/manage/" + slug,
	}
	for _, next := range accepted {
		c := ctxWithForm(url.Values{"next": {next}})
		got := fileOpsNextURL(c, slug, "done")
		if !strings.HasPrefix(got, next) {
			t.Errorf("fileOpsNextURL(next=%q) = %q, want it to honor the allowed destination", next, got)
		}
	}
}

func TestSetProxyCacheHeaders(t *testing.T) {
	t.Parallel()

	t.Run("custom domain is publicly cacheable", func(t *testing.T) {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		c.Set("custom_domain", true)
		setProxyCacheHeaders(c)
		if cc := c.Response().Header().Get("Cache-Control"); cc != "public, max-age=300, s-maxage=3600" {
			t.Errorf("custom-domain Cache-Control = %q", cc)
		}
	})

	t.Run("platform host is never cached", func(t *testing.T) {
		c := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		setProxyCacheHeaders(c)
		if cc := c.Response().Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("platform Cache-Control = %q, want no-store", cc)
		}
	})
}
