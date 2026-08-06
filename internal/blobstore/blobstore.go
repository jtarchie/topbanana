// Package blobstore is the storage contract the auth domain is written
// against, and nothing else. It deliberately imports no other package in this
// repository: that leaf position is what lets the auth packages above it stay
// portable, while the adapter below it (internal/blobs) binds them to this
// platform's object store.
package blobstore

import (
	"context"
	"errors"
)

// Blobs is the entire storage contract the auth domain needs: keyed JSON
// documents, plus a compare-and-set writer. It is deliberately much narrower
// than the object store behind it — no compression, no read cache, no
// per-tenant path rules, no content types — so the auth domain can sit on S3,
// a filesystem, or a test double without knowing or caring which.
//
// The narrowness is the point. Everything above this interface (users,
// sessions, invites, authorization codes) is portable; everything below it is
// somebody's infrastructure.
type Blobs interface {
	// Get returns the object at key. A key that isn't there yields a zero
	// Object and a nil error: a miss is an ordinary answer, not a fault.
	// Conflating the two is how "the store is briefly unreachable" turns into
	// "your credential is invalid" — a terminal verdict on a transient
	// condition.
	Get(ctx context.Context, key string) (Object, error)

	// Put writes content at key unconditionally.
	Put(ctx context.Context, key, content string) error

	// PutIfMatch writes only if the stored object still carries etag, and
	// returns ErrPrecondition if it doesn't. This is what turns
	// read-then-write into an atomic claim: without it, two processes acting
	// on the same key both believe they won, and single-use semantics (an
	// authorization code, a one-time invite) silently aren't.
	PutIfMatch(ctx context.Context, key, content, etag string) error

	// PutIfAbsent writes only if nothing is at key yet, returning
	// ErrPrecondition when another writer got there first.
	PutIfAbsent(ctx context.Context, key, content string) error

	// Delete removes key. Deleting an absent key is not an error.
	Delete(ctx context.Context, key string) error

	// List returns every key carrying prefix.
	List(ctx context.Context, prefix string) ([]string, error)
}

// Object is a stored document plus the version tag needed to write it back
// safely. ETag may be empty on backends that don't version, in which case
// PutIfMatch is unusable and callers relying on atomic claims must say so
// rather than degrade quietly.
type Object struct {
	Content string
	ETag    string
}

// ErrPrecondition reports that a conditional write lost: the object changed,
// appeared, or vanished between the caller's read and its write. It is a
// "someone else won" signal, never a fault — callers turn it into "you didn't
// get the claim", not into a 500.
var ErrPrecondition = errors.New("blobstore: precondition failed")
