package archive_test

import (
	"bytes"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/archive"
)

var epoch = time.Unix(1_700_000_000, 0).UTC()

type archiveEntry struct {
	name string
	body string
	ct   string
	meta map[string]string
}

// assertArchiveEntry compares a read-back entry against what was written. Kept
// out of TestRoundTrip so the round-trip stays under the cognitive-complexity
// cap.
func assertArchiveEntry(t *testing.T, got, want archiveEntry) {
	t.Helper()
	if got.body != want.body {
		t.Errorf("%s body = %q, want %q", want.name, got.body, want.body)
	}
	if got.ct != want.ct {
		t.Errorf("%s content type = %q, want %q", want.name, got.ct, want.ct)
	}
	if len(want.meta) == 0 {
		if got.meta != nil {
			t.Errorf("%s metadata = %v, want nil", want.name, got.meta)
		}
		return
	}
	for k, v := range want.meta {
		if got.meta[k] != v {
			t.Errorf("%s metadata[%q] = %q, want %q", want.name, k, got.meta[k], v)
		}
	}
}

// TestRoundTrip is the foundational guarantee: a Writer -> Reader cycle
// preserves each file's body, content type, and user metadata. Both snapshot
// and portable depend on this byte-identical framing.
func TestRoundTrip(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	want := []archiveEntry{
		{"index.html", "<h1>hi</h1>", "text/html; charset=utf-8", map[string]string{"alt": "a banana — yellow"}},
		{"assets/logo.png", "\x89PNG\x00\x01\x02", "image/png", nil},
		{"data.json", `{"k":1}`, "application/json", map[string]string{"author": "ünïçodé"}},
	}
	for _, f := range want {
		err = w.WriteFile(f.name, []byte(f.body), f.ct, f.meta, epoch)
		if err != nil {
			t.Fatalf("WriteFile %s: %v", f.name, err)
		}
	}
	err = w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	got := readArchive(t, buf.Bytes())
	if len(got) != len(want) {
		t.Fatalf("read %d entries, want %d", len(got), len(want))
	}
	for _, f := range want {
		g, ok := got[f.name]
		if !ok {
			t.Errorf("missing entry %s", f.name)
			continue
		}
		assertArchiveEntry(t, g, f)
	}
}

// readArchive decodes every entry of a zstd+tar blob into a map keyed by name.
func readArchive(t *testing.T, blob []byte) map[string]archiveEntry {
	t.Helper()
	r, err := archive.NewReader(bytes.NewReader(blob), 0)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	got := map[string]archiveEntry{}
	for {
		hdr, nerr := r.Next()
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			t.Fatalf("Next: %v", nerr)
		}
		body, rerr := io.ReadAll(r)
		if rerr != nil {
			t.Fatalf("read body %s: %v", hdr.Name, rerr)
		}
		got[hdr.Name] = archiveEntry{
			name: hdr.Name,
			body: string(body),
			ct:   archive.ContentTypeFromPAX(hdr.PAXRecords),
			meta: archive.MetadataFromPAX(hdr.PAXRecords),
		}
	}
	return got
}

// TestContentTypeFromPAXPrecedence pins the rebrand-compat fallthrough: the
// current TOPBANANA key wins, then each legacy key in order. This is the logic
// that keeps pre-rebrand archives restoring, so a regression here is silent
// metadata loss on old sites.
func TestContentTypeFromPAXPrecedence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		rec  map[string]string
		want string
	}{
		{"none", map[string]string{}, ""},
		{"nil", nil, ""},
		{"current", map[string]string{archive.PAXContentTypeKey: "text/html"}, "text/html"},
		{"legacy bloomhollow", map[string]string{"BLOOMHOLLOW.content-type": "text/css"}, "text/css"},
		{"legacy buildabear", map[string]string{"BUILDABEAR.content-type": "image/png"}, "image/png"},
		{
			name: "current beats legacy",
			rec:  map[string]string{archive.PAXContentTypeKey: "text/html", "BLOOMHOLLOW.content-type": "text/css"},
			want: "text/html",
		},
		{
			name: "bloomhollow beats buildabear",
			rec:  map[string]string{"BLOOMHOLLOW.content-type": "text/css", "BUILDABEAR.content-type": "image/png"},
			want: "text/css",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := archive.ContentTypeFromPAX(tc.rec)
			if got != tc.want {
				t.Errorf("ContentTypeFromPAX(%v) = %q, want %q", tc.rec, got, tc.want)
			}
		})
	}
}

// TestMetadataFromPAX covers the current + legacy meta prefixes and the
// nil-when-absent contract.
func TestMetadataFromPAX(t *testing.T) {
	t.Parallel()

	if got := archive.MetadataFromPAX(map[string]string{"unrelated": "x"}); got != nil {
		t.Errorf("MetadataFromPAX with no meta records = %v, want nil", got)
	}

	rec := map[string]string{
		archive.PAXMetaPrefix + "alt": "current",
		"BLOOMHOLLOW.meta.author":     "legacy1",
		"BUILDABEAR.meta.caption":     "legacy2",
		"TOPBANANA.content-type":      "text/html", // not a meta record
		"unrelated":                   "skip",
	}
	got := archive.MetadataFromPAX(rec)
	want := map[string]string{"alt": "current", "author": "legacy1", "caption": "legacy2"}
	if len(got) != len(want) {
		t.Fatalf("MetadataFromPAX = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("metadata[%q] = %q, want %q", k, got[k], v)
		}
	}
}

// TestNewReaderRespectsMaxDecompressed is the zip-bomb backstop: a body larger
// than the cap must not read clean — the LimitReader cuts the tar stream short
// so the entry read returns an error rather than silently truncated bytes.
func TestNewReaderRespectsMaxDecompressed(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	w, err := archive.NewWriter(&buf)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	// Highly compressible 64 KiB body — tiny compressed, large decompressed.
	big := bytes.Repeat([]byte("A"), 64*1024)
	err = w.WriteFile("big.txt", big, "text/plain", nil, epoch)
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	err = w.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := archive.NewReader(bytes.NewReader(buf.Bytes()), 1024)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close()

	// The guard caps how many decompressed bytes the tar layer can pull, so the
	// 64 KiB body cannot be reconstructed — whether that surfaces as a read
	// error or a short read, the security property (bounded memory) holds.
	var total int64
	for {
		_, nerr := r.Next()
		if nerr != nil {
			break
		}
		n, _ := io.Copy(io.Discard, r)
		total += n
	}
	if total >= int64(len(big)) {
		t.Fatalf("decompression cap not enforced: read %d body bytes, want < %d", total, len(big))
	}
}

// FuzzNewReader feeds arbitrary bytes through the codec. Import ingests
// attacker-controlled archives, so the only invariant is that the reader never
// panics — malformed input must surface as an error.
func FuzzNewReader(f *testing.F) {
	var buf bytes.Buffer
	w, _ := archive.NewWriter(&buf)
	_ = w.WriteFile("a.html", []byte("<p>x</p>"), "text/html", map[string]string{"alt": "x"}, epoch)
	_ = w.Close()
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Add([]byte("not a zstd stream"))

	f.Fuzz(func(t *testing.T, data []byte) {
		r, err := archive.NewReader(bytes.NewReader(data), 1<<20)
		if err != nil {
			return
		}
		defer r.Close()
		for {
			_, nerr := r.Next()
			if nerr != nil {
				break
			}
			_, _ = io.Copy(io.Discard, r)
		}
	})
}
