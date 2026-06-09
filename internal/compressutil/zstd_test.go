package compressutil

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	in := []byte(strings.Repeat("payload that compresses well. ", 200))
	zs, err := Compress(in)
	if err != nil {
		t.Fatalf("Compress: %v", err)
	}
	if len(zs) >= len(in) {
		t.Errorf("did not shrink: in=%d zs=%d", len(in), len(zs))
	}
	if !HasMagic(zs) {
		t.Errorf("compressed output missing magic bytes: %x", zs[:min(4, len(zs))])
	}
	out, err := MaybeDecompress(zs)
	if err != nil {
		t.Fatalf("MaybeDecompress: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("round-trip mismatch (in=%d out=%d)", len(in), len(out))
	}
}

// Verifies the legacy-passthrough contract: a plaintext payload (the shape of
// every pre-compression object in S3) round-trips through MaybeDecompress
// unchanged. This is what lets us deploy compression without a migration.
func TestPassthrough(t *testing.T) {
	for _, in := range [][]byte{
		[]byte(`{"slug":"x"}`),
		[]byte(`<!doctype html><html></html>`),
		[]byte(`.foo { color: red; }`),
		nil,
		{},
		{0x28}, // single byte of the magic — too short to be valid, must pass through
	} {
		out, err := MaybeDecompress(in)
		if err != nil {
			t.Errorf("MaybeDecompress(%q): %v", in, err)
			continue
		}
		if !bytes.Equal(out, in) {
			t.Errorf("passthrough mismatch: %q vs %q", out, in)
		}
	}
}
