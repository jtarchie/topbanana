// Package archive is the tar+zstd codec shared by internal/snapshot (per-site
// version history) and internal/portable (cross-instance export/import). Both
// wire formats are a zstd-compressed tar in which each entry carries its S3
// content-type and user metadata as PAX records. This package owns that PAX
// key contract — including the legacy pre-rebrand prefixes older archives still
// use — and the tar+zstd plumbing, so the two callers emit byte-identical
// framing and differ only in their higher-level orchestration (snapshot keys by
// slug; portable adds a manifest entry and enforces import caps).
package archive

import (
	"archive/tar"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"
)

// PAX header key prefixes for per-file content type and S3 user metadata. New
// archives are always written with the TOPBANANA.* prefix.
const (
	PAXContentTypeKey = "TOPBANANA.content-type"
	PAXMetaPrefix     = "TOPBANANA.meta."
)

// Legacy PAX prefixes from before each rebrand, newest first. ContentTypeFromPAX
// and MetadataFromPAX fall through these so archives written before a rebrand
// still restore cleanly: BLOOMHOLLOW.* predates Top Banana, BUILDABEAR.*
// predates Bloomhollow.
var (
	LegacyPAXContentTypeKeys = []string{"BLOOMHOLLOW.content-type", "BUILDABEAR.content-type"}
	LegacyPAXMetaPrefixes    = []string{"BLOOMHOLLOW.meta.", "BUILDABEAR.meta."}
)

// ContentTypeFromPAX returns the per-file content type recorded in a tar
// header's PAX records, checking the current key then each legacy key in
// order. Returns "" when none is present (caller should fall back to the file
// extension).
func ContentTypeFromPAX(rec map[string]string) string {
	if ct := rec[PAXContentTypeKey]; ct != "" {
		return ct
	}
	for _, k := range LegacyPAXContentTypeKeys {
		if ct := rec[k]; ct != "" {
			return ct
		}
	}
	return ""
}

// MetadataFromPAX extracts S3 user metadata from a tar header's PAX records,
// honouring the current meta prefix and each legacy prefix. Returns nil when
// no metadata records are present.
func MetadataFromPAX(rec map[string]string) map[string]string {
	var metadata map[string]string
	prefixes := append([]string{PAXMetaPrefix}, LegacyPAXMetaPrefixes...)
	for k, v := range rec {
		for _, p := range prefixes {
			rest, ok := strings.CutPrefix(k, p)
			if !ok {
				continue
			}
			if metadata == nil {
				metadata = map[string]string{}
			}
			metadata[rest] = v
			break
		}
	}
	return metadata
}

// Writer streams files into a zstd-compressed tar. Create with NewWriter, add
// entries with WriteFile, then Close (which flushes the tar then the zstd
// layer). The zero value is unusable.
type Writer struct {
	tw *tar.Writer
	zw *zstd.Encoder
}

// NewWriter wraps w in a zstd encoder + tar writer.
func NewWriter(w io.Writer) (*Writer, error) {
	zw, err := zstd.NewWriter(w)
	if err != nil {
		return nil, fmt.Errorf("init zstd: %w", err)
	}
	return &Writer{tw: tar.NewWriter(zw), zw: zw}, nil
}

// WriteFile appends one regular file, recording its content type and metadata
// as PAX records under the current TOPBANANA.* keys.
func (w *Writer) WriteFile(name string, body []byte, contentType string, metadata map[string]string, ts time.Time) error {
	header := &tar.Header{
		Name:    name,
		Size:    int64(len(body)),
		Mode:    0644,
		ModTime: ts,
	}
	if contentType != "" {
		header.PAXRecords = map[string]string{PAXContentTypeKey: contentType}
	}
	if len(metadata) > 0 {
		if header.PAXRecords == nil {
			header.PAXRecords = map[string]string{}
		}
		for k, v := range metadata {
			header.PAXRecords[PAXMetaPrefix+k] = v
		}
	}
	err := w.tw.WriteHeader(header)
	if err != nil {
		return fmt.Errorf("tar header %s: %w", name, err)
	}
	_, err = w.tw.Write(body)
	if err != nil {
		return fmt.Errorf("tar write %s: %w", name, err)
	}
	return nil
}

// Close flushes and closes both the tar and zstd layers.
func (w *Writer) Close() error {
	err := w.tw.Close()
	if err != nil {
		return fmt.Errorf("close tar: %w", err)
	}
	err = w.zw.Close()
	if err != nil {
		return fmt.Errorf("close zstd: %w", err)
	}
	return nil
}

// Reader is a zstd-decompressing tar reader. It embeds *tar.Reader so callers
// run their own entry loop (each keeps its own caps / manifest / reserved-path
// rules) and decode per-entry PAX with ContentTypeFromPAX / MetadataFromPAX.
type Reader struct {
	*tar.Reader
	zr *zstd.Decoder
}

// NewReader decompresses r (zstd) and exposes the tar stream. maxDecompressed,
// when > 0, caps the total bytes the tar layer will read — a zip-bomb backstop
// for untrusted archives; pass 0 for trusted, self-produced archives.
func NewReader(r io.Reader, maxDecompressed int64) (*Reader, error) {
	zr, err := zstd.NewReader(r)
	if err != nil {
		return nil, err //nolint:wrapcheck // caller pairs this with its own sentinel
	}
	var src io.Reader = zr
	if maxDecompressed > 0 {
		src = io.LimitReader(zr, maxDecompressed)
	}
	return &Reader{Reader: tar.NewReader(src), zr: zr}, nil
}

// Close releases the underlying zstd decoder.
func (r *Reader) Close() { r.zr.Close() }
