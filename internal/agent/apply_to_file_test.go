package agent

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jtarchie/topbanana/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.NewInMemory(0)
	if err != nil {
		t.Fatalf("store.NewInMemory: %v", err)
	}
	return s
}

func TestApplyToFile_Success(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	const slug, path = "site", "index.html"
	err := s.Write(ctx, slug, path, "<p>old</p>", "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatal(err)
	}
	state := newBuildState()

	before, after, err := applyToFile(ctx, s, slug, path, "edit_file", "sig", state, func(c string) (string, error) {
		return strings.Replace(c, "old", "new", 1), nil
	})
	if err != nil {
		t.Fatalf("applyToFile: %v", err)
	}
	if before != "<p>old</p>" || after != "<p>new</p>" {
		t.Fatalf("before=%q after=%q", before, after)
	}
	obj, _ := s.Read(ctx, slug, path)
	if obj.Content != "<p>new</p>" {
		t.Fatalf("stored content = %q, want <p>new</p>", obj.Content)
	}
}

func TestApplyToFile_FileNotFound(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	state := newBuildState()
	_, _, err := applyToFile(ctx, s, "site", "missing.html", "edit_file", "sig", state, func(c string) (string, error) {
		return c, nil
	})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want a file-not-found error, got %v", err)
	}
}

func TestApplyToFile_TransformErrorLeavesFileUnchanged(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Write(ctx, "site", "i.html", "original", "text/html; charset=utf-8", nil)
	state := newBuildState()

	_, _, err := applyToFile(ctx, s, "site", "i.html", "edit_file", "sig", state, func(string) (string, error) {
		return "", errors.New("boom")
	})
	if err == nil {
		t.Fatal("want the transform error to propagate")
	}
	obj, _ := s.Read(ctx, "site", "i.html")
	if obj.Content != "original" {
		t.Fatalf("file mutated despite transform error: %q", obj.Content)
	}
}

func TestApplyToFile_SizeCapBlocksWrite(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Write(ctx, "site", "i.html", "small", "text/html; charset=utf-8", nil)
	state := newBuildState()

	_, _, err := applyToFile(ctx, s, "site", "i.html", "edit_file", "sig", state, func(string) (string, error) {
		return strings.Repeat("a", maxHTMLFileBytes+1), nil
	})
	if err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("want a size-cap error, got %v", err)
	}
	obj, _ := s.Read(ctx, "site", "i.html")
	if obj.Content != "small" {
		t.Fatal("file mutated despite exceeding the size cap")
	}
}

func TestApplyToFile_PreservesContentType(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Write(ctx, "site", "icon.svg", "<svg></svg>", "image/svg+xml", nil)
	state := newBuildState()

	_, _, err := applyToFile(ctx, s, "site", "icon.svg", "edit_file", "sig", state, func(c string) (string, error) {
		return c + "<!-- edited -->", nil
	})
	if err != nil {
		t.Fatalf("applyToFile: %v", err)
	}
	obj, _ := s.Read(ctx, "site", "icon.svg")
	if obj.ContentType != "image/svg+xml" {
		t.Fatalf("content type not preserved: %q", obj.ContentType)
	}
}

func TestApplyToFile_GuardBlocksDuplicateSignature(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Write(ctx, "site", "i.html", "abc", "text/html; charset=utf-8", nil)
	state := newBuildState()
	noop := func(c string) (string, error) { return c + "x", nil }

	_, _, err := applyToFile(ctx, s, "site", "i.html", "edit_file", "dupe", state, noop)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	_, _, err = applyToFile(ctx, s, "site", "i.html", "edit_file", "dupe", state, noop)
	if err == nil {
		t.Fatal("the anti-loop guard should reject an identical signature on the second call")
	}
}

// TestApplyToFile_WriteMuSerializes is the real serialization assertion the old
// hand-rolled concurrency test couldn't make: 16 goroutines each append one
// byte via a read-modify-write. Without writeMu, concurrent Read+Write would
// lose updates (two goroutines read the same content and both write their own
// single-byte append); with it, all 16 land, so the file grows by exactly 16.
func TestApplyToFile_WriteMuSerializes(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	_ = s.Write(ctx, "site", "i.html", "0", "text/html; charset=utf-8", nil)
	state := newBuildState()

	const n = 16
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct signatures so the anti-loop guard allows every call.
			_, _, _ = applyToFile(ctx, s, "site", "i.html", "edit_file", "sig"+strconv.Itoa(i), state, func(c string) (string, error) {
				return c + "x", nil
			})
		}(i)
	}
	wg.Wait()

	obj, _ := s.Read(ctx, "site", "i.html")
	if len(obj.Content) != 1+n {
		t.Fatalf("lost concurrent writes: content len = %d, want %d", len(obj.Content), 1+n)
	}
}
