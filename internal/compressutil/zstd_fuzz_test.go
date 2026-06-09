package compressutil

import (
	"bytes"
	"testing"
)

// FuzzCompressRoundTrip asserts MaybeDecompress(Compress(b)) == b for arbitrary
// input, including content that itself begins with the zstd magic bytes — the
// boundary the HasMagic sniff has to get right so a plaintext payload that
// happens to start with 0x28 0xb5 0x2f 0xfd still round-trips.
func FuzzCompressRoundTrip(f *testing.F) {
	for _, b := range [][]byte{
		nil,
		{},
		[]byte("hello"),
		[]byte("<p>repeatable</p>"),
		{0x28, 0xb5, 0x2f, 0xfd, 'n', 'o', 't', 'z', 's', 't', 'd'},
	} {
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		comp, err := Compress(b)
		if err != nil {
			t.Fatalf("Compress: %v", err)
		}
		out, err := MaybeDecompress(comp)
		if err != nil {
			t.Fatalf("MaybeDecompress: %v", err)
		}
		if !bytes.Equal(out, b) && (len(out) != 0 || len(b) != 0) {
			t.Fatalf("round-trip mismatch: got %q want %q", out, b)
		}
	})
}
