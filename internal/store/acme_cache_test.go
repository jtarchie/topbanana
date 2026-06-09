package store_test

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/acme/autocert"

	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

func TestACMECache_RoundTrip(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()
	// Unique prefix per run so parallel tests / leftover state don't collide.
	prefix := "_acme-test-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "/"
	cache := store.NewACMECache(s, prefix)

	t.Cleanup(func() {
		_ = cache.Delete(ctx, "round-trip")
		_ = cache.Delete(ctx, "binary")
	})

	// Miss before any write.
	_, err := cache.Get(ctx, "round-trip")
	if !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss, got %v", err)
	}

	// Put then Get.
	want := []byte("account+key payload")
	err = cache.Put(ctx, "round-trip", want)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := cache.Get(ctx, "round-trip")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("round-trip mismatch: got %q want %q", got, want)
	}

	// Binary safety: cert/key blobs are DER-encoded, must survive
	// string<->[]byte conversion in the store.
	binary := []byte{0x00, 0x01, 0x02, 0xff, 0xfe, 0xfd, 0x7f, 0x80}
	err = cache.Put(ctx, "binary", binary)
	if err != nil {
		t.Fatalf("put binary: %v", err)
	}
	gotBin, err := cache.Get(ctx, "binary")
	if err != nil {
		t.Fatalf("get binary: %v", err)
	}
	if !bytes.Equal(gotBin, binary) {
		t.Fatalf("binary round-trip mismatch: got % x want % x", gotBin, binary)
	}

	// Delete then Get reports miss again.
	err = cache.Delete(ctx, "round-trip")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	_, err = cache.Get(ctx, "round-trip")
	if !errors.Is(err, autocert.ErrCacheMiss) {
		t.Fatalf("expected ErrCacheMiss after delete, got %v", err)
	}
}

func TestACMECache_PrefixNormalization(t *testing.T) {
	cache := store.NewACMECache(nil, "_acme")
	if cache.Prefix != "_acme/" {
		t.Errorf("expected trailing slash to be added, got %q", cache.Prefix)
	}
	cache = store.NewACMECache(nil, "_acme/")
	if cache.Prefix != "_acme/" {
		t.Errorf("expected idempotent normalization, got %q", cache.Prefix)
	}
}
