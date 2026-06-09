package store_test

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/storetest"
)

// TestStore_ConcurrentSamePathWrites documents what S3 actually gives us when
// the same (slug, path) is written by N goroutines at once. The contract we
// need from the store is: no torn write, no panic, the final read returns one
// of the bodies intact. Ordering is *not* asserted — ordering is the agent's
// problem (handled by buildState.writeMu in internal/agent).
//
// Skips when minio env vars aren't set, same as the other store tests.
func TestStore_ConcurrentSamePathWrites(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()
	slug := "concurrent-write-test-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	path := "index.html"
	t.Cleanup(func() {
		_ = s.Delete(ctx, slug, path)
	})

	const writers = 8
	bodies := make([]string, writers)
	for i := range bodies {
		bodies[i] = strings.Repeat("payload-"+strconv.Itoa(i)+" ", 256)
	}

	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range bodies {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			err := s.Write(ctx, slug, path, body, "text/html; charset=utf-8", nil)
			if err != nil {
				errs <- err
			}
		}(bodies[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent write: %v", err)
	}

	obj, err := s.Read(ctx, slug, path)
	if err != nil {
		t.Fatalf("read after concurrent writes: %v", err)
	}
	matched := false
	for _, body := range bodies {
		if obj.Content == body {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("final content does not match any written body (len=%d, prefix=%q)",
			len(obj.Content), obj.Content[:min(40, len(obj.Content))])
	}
}
