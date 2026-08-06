// Package blobtest is the conformance suite for blob.Blobs implementations.
//
// It ships with the contract because the three semantics that matter most are
// the three easiest to get subtly wrong, and each failure mode is silent: a
// miss reported as an error turns a brief outage into "your credential is
// invalid"; a missing-key If-Match that doesn't report ErrPrecondition breaks
// the single-use claim; an empty ETag makes compare-and-set a no-op that still
// returns success. None of those produce a compile error, and the auth code
// above will look like it works.
//
// Import from a _test file and call Run with a factory for your adapter.
package blobtest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/auth/blob"
)

// keySeq keeps keys unique within a run; the timestamp keeps them unique
// across runs, which matters when the implementation is a shared bucket that
// nothing cleans between invocations.
var keySeq atomic.Uint64

func freshPrefix(t *testing.T) string {
	t.Helper()
	return "blobtest/" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatUint(keySeq.Add(1), 10) + "/"
}

// Run drives every implementation-independent guarantee of blob.Blobs against
// the store newStore returns. Safe to call against a shared remote bucket:
// every check namespaces its own keys.
func Run(t *testing.T, newStore func() blob.Blobs) {
	t.Helper()
	for _, c := range checks {
		t.Run(c.name, func(t *testing.T) { c.fn(t, newStore()) })
	}
}

// checks is the suite. Kept as a table so a reader can see the whole contract
// at a glance and so each guarantee stays a named, separately-failing case.
var checks = []struct {
	name string
	fn   func(*testing.T, blob.Blobs)
}{
	{"miss is not an error", checkMissIsNotAnError},
	{"put round trips and carries an etag", checkPutRoundTrips},
	{"put if absent admits one writer", checkPutIfAbsent},
	{"put if match rejects a stale etag", checkPutIfMatchStale},
	{"put if match on a missing key loses", checkPutIfMatchMissing},
	{"delete is idempotent and removes", checkDelete},
	{"list returns keys under the prefix", checkList},
	{"concurrent claim yields exactly one winner", checkConcurrentClaim},
}

func checkMissIsNotAnError(t *testing.T, s blob.Blobs) {
	obj, err := s.Get(context.Background(), freshPrefix(t)+"absent.json")
	if err != nil {
		t.Fatalf("Get on a missing key returned an error: %v", err)
	}
	if obj.Content != "" {
		t.Fatalf("Get on a missing key returned content %q", obj.Content)
	}
}

func checkPutRoundTrips(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	key := freshPrefix(t) + "doc.json"
	err := s.Put(ctx, key, `{"a":1}`)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	obj, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if obj.Content != `{"a":1}` {
		t.Fatalf("content = %q, want the written body", obj.Content)
	}
	if obj.ETag == "" {
		t.Fatal("ETag is empty — PutIfMatch cannot work, so every single-use guarantee built on this store is void")
	}
}

func checkPutIfAbsent(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	key := freshPrefix(t) + "once.json"
	err := s.PutIfAbsent(ctx, key, "first")
	if err != nil {
		t.Fatalf("first PutIfAbsent: %v", err)
	}
	err = s.PutIfAbsent(ctx, key, "second")
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("second PutIfAbsent = %v, want ErrPrecondition", err)
	}
	obj, _ := s.Get(ctx, key)
	if obj.Content != "first" {
		t.Fatalf("content = %q, want the first writer's body", obj.Content)
	}
}

func checkPutIfMatchStale(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	key := freshPrefix(t) + "cas.json"
	err := s.Put(ctx, key, "v1")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	first, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	err = s.PutIfMatch(ctx, key, "v2", first.ETag)
	if err != nil {
		t.Fatalf("PutIfMatch with a current ETag: %v", err)
	}
	err = s.PutIfMatch(ctx, key, "v3", first.ETag)
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("PutIfMatch with a stale ETag = %v, want ErrPrecondition", err)
	}
	obj, _ := s.Get(ctx, key)
	if obj.Content != "v2" {
		t.Fatalf("content = %q, want the winner's body", obj.Content)
	}
}

// S3 and Minio both answer 404 NoSuchKey here rather than 412. An adapter that
// passes the raw error through breaks callers that treat ErrPrecondition as
// the single "you lost" signal.
func checkPutIfMatchMissing(t *testing.T, s blob.Blobs) {
	err := s.PutIfMatch(context.Background(), freshPrefix(t)+"gone.json", "x", "some-etag")
	if !errors.Is(err, blob.ErrPrecondition) {
		t.Fatalf("PutIfMatch on a missing key = %v, want ErrPrecondition", err)
	}
}

func checkDelete(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	key := freshPrefix(t) + "doomed.json"
	err := s.Delete(ctx, key)
	if err != nil {
		t.Fatalf("Delete on an absent key: %v", err)
	}
	err = s.Put(ctx, key, "here")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	err = s.Delete(ctx, key)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	obj, err := s.Get(ctx, key)
	if err != nil || obj.Content != "" {
		t.Fatalf("after Delete, Get = (%q, %v), want empty and nil", obj.Content, err)
	}
}

func checkList(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	prefix := freshPrefix(t)
	for _, name := range []string{"a.json", "b.json"} {
		err := s.Put(ctx, prefix+name, "{}")
		if err != nil {
			t.Fatalf("Put %s: %v", name, err)
		}
	}
	err := s.Put(ctx, freshPrefix(t)+"elsewhere.json", "{}")
	if err != nil {
		t.Fatalf("Put outside prefix: %v", err)
	}
	keys, err := s.List(ctx, prefix)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("List returned %v, want exactly the 2 keys under %s", keys, prefix)
	}
}

// The reason PutIfMatch exists. A store that lets two claimants win here will
// pass every other check in this suite and still hand out two tokens for one
// authorization code.
func checkConcurrentClaim(t *testing.T, s blob.Blobs) {
	ctx := context.Background()
	key := freshPrefix(t) + "claim.json"
	err := s.Put(ctx, key, "unclaimed")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	start, err := s.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

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
			cerr := s.PutIfMatch(ctx, key, "claimed-by-"+strconv.Itoa(i), start.ETag)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case cerr == nil:
				winners++
			case errors.Is(cerr, blob.ErrPrecondition):
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
		t.Fatalf("%d claimants won, want exactly 1", winners)
	}
}
