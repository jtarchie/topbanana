package photowall

import (
	"strings"
	"testing"
)

func TestMetaKeyAndParseID(t *testing.T) {
	t.Parallel()

	key := MetaKey("00000042")
	if key != "photo:00000042" {
		t.Fatalf("MetaKey = %q, want photo:00000042", key)
	}
	if !IsPhotoKey(key) {
		t.Fatalf("IsPhotoKey(%q) = false", key)
	}
	id, ok := ParseID(key)
	if !ok || id != "00000042" {
		t.Fatalf("ParseID(%q) = %q,%v want 00000042,true", key, id, ok)
	}

	for _, bad := range []string{"seq", "photo_seq", "submission:1", "photo:"} {
		if _, ok := ParseID(bad); ok {
			t.Errorf("ParseID(%q) = ok, want not-ok", bad)
		}
	}
}

func TestFormatID(t *testing.T) {
	t.Parallel()

	cases := map[int64]string{1: "00000001", 42: "00000042", 12345678: "12345678"}
	for seq, want := range cases {
		if got := FormatID(seq); got != want {
			t.Errorf("FormatID(%d) = %q, want %q", seq, got, want)
		}
	}
	// Lexical order must match numeric order for the queue's descending sort.
	if FormatID(9) >= FormatID(10) {
		t.Errorf("FormatID(9) should sort before FormatID(10)")
	}
}

func TestPaths(t *testing.T) {
	t.Parallel()

	if got := PendingPath("00000042", ".jpg"); got != "_pending/00000042.jpg" {
		t.Errorf("PendingPath = %q", got)
	}
	if got := ApprovedPath("00000042", ".jpg"); got != "assets/photowall/00000042.jpg" {
		t.Errorf("ApprovedPath = %q", got)
	}
}

func TestExtForContentTypeAndAllowedExt(t *testing.T) {
	t.Parallel()

	accepted := map[string]string{
		"image/jpeg": ".jpg",
		"image/png":  ".png",
		"image/gif":  ".gif",
		"image/webp": ".webp",
	}
	for ct, ext := range accepted {
		got, ok := ExtForContentType(ct)
		if !ok || got != ext {
			t.Errorf("ExtForContentType(%q) = %q,%v want %q,true", ct, got, ok, ext)
		}
		if !AllowedExt(ext) {
			t.Errorf("AllowedExt(%q) = false", ext)
		}
	}
	// SVG is deliberately excluded from the photo wall.
	if _, ok := ExtForContentType("image/svg+xml"); ok {
		t.Errorf("ExtForContentType(image/svg+xml) accepted; SVG must be excluded")
	}
	for _, bad := range []string{"text/plain", "application/pdf", ".svg", ".txt", ""} {
		if AllowedExt(bad) {
			t.Errorf("AllowedExt(%q) = true, want false", bad)
		}
	}
}

func TestCanTransition(t *testing.T) {
	t.Parallel()

	if !CanTransition(StatusPending, StatusApproved) {
		t.Errorf("pending->approved must be allowed")
	}
	for _, tc := range []struct{ from, to string }{
		{StatusApproved, StatusPending},
		{StatusApproved, StatusApproved},
		{StatusPending, StatusPending},
		{"", StatusApproved},
		{StatusPending, "deleted"},
	} {
		if CanTransition(tc.from, tc.to) {
			t.Errorf("CanTransition(%q,%q) = true, want false", tc.from, tc.to)
		}
	}
}

func TestFromMetaToMetaRoundTrip(t *testing.T) {
	t.Parallel()

	p := Photo{ID: "00000042", Status: StatusApproved, Ext: ".jpg", Asset: "assets/photowall/00000042.jpg", TS: 1720000000000}
	m := p.ToMeta()
	// Simulate a JSON round-trip: ts comes back as float64.
	m["ts"] = float64(p.TS)

	got, ok := FromMeta(m)
	if !ok {
		t.Fatalf("FromMeta returned not-ok")
	}
	if got != p {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, p)
	}
}

func TestToMetaOmitsAssetWhilePending(t *testing.T) {
	t.Parallel()

	p := Photo{ID: "00000001", Status: StatusPending, Ext: ".png", TS: 1}
	m := p.ToMeta()
	if _, ok := m["asset"]; ok {
		t.Errorf("pending row must not carry an asset path: %+v", m)
	}
}

func TestFromMetaRejectsMalformed(t *testing.T) {
	t.Parallel()

	cases := []map[string]any{
		nil,
		{},                                  // no id/status
		{"id": "1"},                         // missing status
		{"id": "1", "status": "bogus"},      // unknown status
		{"status": StatusPending},           // missing id
		{"id": "", "status": StatusPending}, // empty id
	}
	for i, m := range cases {
		if _, ok := FromMeta(m); ok {
			t.Errorf("case %d: FromMeta(%+v) = ok, want not-ok", i, m)
		}
	}
}

func TestCountPendingAndCollect(t *testing.T) {
	t.Parallel()

	data := map[string]any{
		"photo_seq":         float64(3),
		"submission:0001":   map[string]any{"name": "x"}, // not a photo row
		MetaKey("00000001"): Photo{ID: "00000001", Status: StatusPending, Ext: ".jpg", TS: 1}.ToMeta(),
		MetaKey("00000002"): Photo{ID: "00000002", Status: StatusApproved, Ext: ".jpg", Asset: "assets/photowall/00000002.jpg", TS: 2}.ToMeta(),
		MetaKey("00000003"): Photo{ID: "00000003", Status: StatusPending, Ext: ".png", TS: 3}.ToMeta(),
	}

	if n := CountPending(data); n != 2 {
		t.Errorf("CountPending = %d, want 2", n)
	}

	pending := Collect(data, StatusPending)
	if len(pending) != 2 {
		t.Fatalf("Collect(pending) = %d rows, want 2", len(pending))
	}
	// Newest-first: id 3 before id 1.
	if pending[0].ID != "00000003" || pending[1].ID != "00000001" {
		t.Errorf("Collect not newest-first: %q, %q", pending[0].ID, pending[1].ID)
	}

	approved := Collect(data, StatusApproved)
	if len(approved) != 1 || approved[0].ID != "00000002" {
		t.Errorf("Collect(approved) = %+v, want single id 00000002", approved)
	}
}

func TestLimiterAllowsBurstThenDenies(t *testing.T) {
	t.Parallel()

	// 0 rps so no tokens refill within the test window; burst of 3.
	l := NewLimiter(0.0001, 3)
	key := "slug|1.2.3.4"

	for i := range 3 {
		if !l.Allow(key) {
			t.Fatalf("burst token %d denied", i+1)
		}
	}
	if l.Allow(key) {
		t.Errorf("4th request within burst window should be denied")
	}
	// A different key has its own bucket.
	if !l.Allow("slug|5.6.7.8") {
		t.Errorf("independent key should have its own tokens")
	}
}

func TestQRCodeSVG(t *testing.T) {
	t.Parallel()

	svg, err := QRCodeSVG("https://lush-shore-7.apps.topbanana.dev/")
	if err != nil {
		t.Fatalf("QRCodeSVG: %v", err)
	}
	for _, want := range []string{"<svg", "viewBox", "</svg>", "<path"} {
		if !strings.Contains(svg, want) {
			t.Errorf("SVG missing %q:\n%s", want, svg[:min(len(svg), 200)])
		}
	}
	// No external references — the hosted page must stay self-contained.
	if strings.Contains(svg, "http://www.w3.org") {
		// The xmlns is allowed; anything else fetching a remote resource is not.
		if strings.Contains(svg, "xlink") || strings.Contains(svg, "<image") {
			t.Errorf("SVG must not reference external resources")
		}
	}
	// Deterministic: same content encodes identically.
	again, _ := QRCodeSVG("https://lush-shore-7.apps.topbanana.dev/")
	if svg != again {
		t.Errorf("QRCodeSVG not deterministic for the same content")
	}
	// Different content encodes differently.
	other, _ := QRCodeSVG("https://example.com/")
	if svg == other {
		t.Errorf("different URLs produced identical QR SVG")
	}
}
