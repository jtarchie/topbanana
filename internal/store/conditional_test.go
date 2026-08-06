package store_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

// The conditional writers are the primitive that makes a cross-process claim
// atomic — "read it, then act on it" is only single-use if exactly one reader
// can win the write. These run against the in-memory backend by default and
// against real Minio when AWS_ENDPOINT_URL + S3_BUCKET are set, which is the
// point: the semantics have to be identical or the in-memory tests are lying.

func condKey(t *testing.T) string {
	t.Helper()
	return "_auth/conditional-test/" + strconv.FormatInt(time.Now().UnixNano(), 36) + ".json"
}

func TestStore_WriteRawIfAbsent_OnlyFirstWriterWins(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()
	key := condKey(t)
	t.Cleanup(func() { _ = s.DeleteRaw(ctx, key) })

	_, err := s.WriteRawIfAbsent(ctx, key, `{"n":1}`, "application/json", nil)
	if err != nil {
		t.Fatalf("first WriteRawIfAbsent: %v", err)
	}

	_, err = s.WriteRawIfAbsent(ctx, key, `{"n":2}`, "application/json", nil)
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("second WriteRawIfAbsent err = %v, want ErrPrecondition", err)
	}

	obj, err := s.ReadRaw(ctx, key)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if obj.Content != `{"n":1}` {
		t.Fatalf("content = %q, want the first writer's body", obj.Content)
	}
}

func TestStore_WriteRawIfMatch_StaleETagLoses(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()
	key := condKey(t)
	t.Cleanup(func() { _ = s.DeleteRaw(ctx, key) })

	err := s.WriteRaw(ctx, key, `{"n":1}`, "application/json", nil)
	if err != nil {
		t.Fatalf("WriteRaw: %v", err)
	}
	first, err := s.ReadRaw(ctx, key)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if first.ETag == "" {
		t.Fatal("backend returned no ETag — compare-and-set cannot work without one")
	}

	_, err = s.WriteRawIfMatch(ctx, key, `{"n":2}`, "application/json", nil, first.ETag)
	if err != nil {
		t.Fatalf("WriteRawIfMatch with a current ETag: %v", err)
	}

	// The ETag we captured is now stale; a second writer holding it must lose.
	_, err = s.WriteRawIfMatch(ctx, key, `{"n":3}`, "application/json", nil, first.ETag)
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("stale-ETag write err = %v, want ErrPrecondition", err)
	}

	obj, err := s.ReadRaw(ctx, key)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}
	if obj.Content != `{"n":2}` {
		t.Fatalf("content = %q, want the winner's body", obj.Content)
	}
}

func TestStore_WriteRawIfMatch_MissingKeyLoses(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()

	_, err := s.WriteRawIfMatch(ctx, condKey(t), "{}", "application/json", nil, "some-etag")
	if !errors.Is(err, store.ErrPrecondition) {
		t.Fatalf("If-Match against a missing key err = %v, want ErrPrecondition", err)
	}
}

// TestStore_ConditionalClaim_ExactlyOneWinner is the property the OAuth code
// path depends on: N processes read the same object and race to claim it, and
// exactly one may proceed. A plain read-then-write gives N winners.
func TestStore_ConditionalClaim_ExactlyOneWinner(t *testing.T) {
	s := storetest.New(t, 0)
	ctx := context.Background()
	key := condKey(t)
	t.Cleanup(func() { _ = s.DeleteRaw(ctx, key) })

	err := s.WriteRaw(ctx, key, `{"claimed":false}`, "application/json", nil)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	obj, err := s.ReadRaw(ctx, key)
	if err != nil {
		t.Fatalf("ReadRaw: %v", err)
	}

	// Every claimant read the object at the same version, exactly as two
	// instances handling a retried token exchange would.
	const claimants = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		other   error
	)
	for i := range claimants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, cerr := s.WriteRawIfMatch(ctx, key, `{"claimed":true,"by":`+strconv.Itoa(i)+`}`, "application/json", nil, obj.ETag)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case cerr == nil:
				winners++
			case errors.Is(cerr, store.ErrPrecondition):
			default:
				other = cerr
			}
		}()
	}
	wg.Wait()

	if other != nil {
		t.Fatalf("unexpected error from a claimant: %v", other)
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}
}
