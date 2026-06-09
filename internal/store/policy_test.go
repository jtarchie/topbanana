package store

import (
	"testing"
)

// TestIsCompressibleContentType pins the compression allowlist. New mime types
// the platform stores should be considered here — if a future caller writes a
// type the platform never stored before, we want a deliberate decision rather
// than a silent default.
func TestIsCompressibleContentType(t *testing.T) {
	for _, ct := range []string{
		"text/html",
		"text/html; charset=utf-8",
		"TEXT/HTML",
		"  text/css  ",
		"text/plain",
		"application/json",
		"application/json; charset=utf-8",
		"application/javascript",
		"application/xml",
		"image/svg+xml",
	} {
		if !isCompressibleContentType(ct) {
			t.Errorf("expected %q compressible", ct)
		}
	}
	for _, ct := range []string{
		"image/png",
		"image/jpeg",
		"image/gif",
		"image/webp",
		"font/woff2",
		"application/octet-stream",
		"application/zstd",
		"",
	} {
		if isCompressibleContentType(ct) {
			t.Errorf("expected %q NOT compressible", ct)
		}
	}
}
