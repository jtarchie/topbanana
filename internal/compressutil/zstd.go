// Package compressutil wraps klauspost/compress/zstd with two helpers used by
// every package that compresses bytes at rest in S3 (editrec transcripts,
// site files via the store layer, future callers). Centralized so the magic
// constant, the encoder reuse pattern, and the legacy-passthrough policy
// (return input unchanged when the magic bytes are absent) live in one place.
package compressutil

import (
	"fmt"

	"github.com/klauspost/compress/zstd"
)

// Magic is the 4-byte zstd frame magic (RFC 8878). MaybeDecompress uses it to
// distinguish compressed payloads from legacy plaintext, which never starts
// with this byte sequence in any of the formats we store (JSON: '{', '['; HTML:
// '<'; CSS: '/' '@' '.' '*' or whitespace).
var Magic = [4]byte{0x28, 0xb5, 0x2f, 0xfd}

// Compress returns the zstd-encoded bytes of in. Uses a fresh stateless
// encoder per call — allocation cost is small relative to the S3 round-trip
// that always follows, and avoiding a shared encoder keeps the API safe to
// call from any goroutine without synchronization.
func Compress(in []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd writer: %w", err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(in, nil), nil
}

// MaybeDecompress returns the input unchanged unless it starts with the zstd
// magic bytes, in which case it decompresses and returns the original payload.
// This is the entire backward-compatibility story: pre-compression objects
// (raw JSON, HTML, etc.) pass through; compressed objects decode. No version
// flag or schema field needed.
func MaybeDecompress(in []byte) ([]byte, error) {
	if !HasMagic(in) {
		return in, nil
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, fmt.Errorf("zstd reader: %w", err)
	}
	defer dec.Close()
	out, err := dec.DecodeAll(in, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decode: %w", err)
	}
	return out, nil
}

// HasMagic reports whether b starts with the zstd frame magic. Exposed so
// tests can assert "this was stored compressed" without round-tripping.
func HasMagic(b []byte) bool {
	return len(b) >= 4 && b[0] == Magic[0] && b[1] == Magic[1] && b[2] == Magic[2] && b[3] == Magic[3]
}
