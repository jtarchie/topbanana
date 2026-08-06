// Package blobs adapts the platform's object store to blob.Blobs, the
// narrow keyed-document contract the auth domain and the MCP OAuth server are
// written against.
//
// It exists so those packages never name *store.Store. That is the whole seam:
// everything above it is portable auth logic, everything below it is this
// platform's storage — compression at rest, the ARC cache, slug-prefix
// validation — none of which the auth domain has any business knowing about.
// Keeping the adapter here rather than in internal/auth means the dependency
// points the right way: the consumer adapts itself to the library, not the
// reverse.
package blobs

import (
	"context"
	"errors"
	"fmt"

	"github.com/jtarchie/topbanana/auth/blob"
	"github.com/jtarchie/topbanana/internal/store"
)

// contentType is what every document this adapter writes carries. The auth
// domain only ever stores JSON, so the contract doesn't take one.
const contentType = "application/json"

// Store adapts *store.Store to blob.Blobs.
type Store struct{ s *store.Store }

// FromStore wraps s. The returned value satisfies blob.Blobs.
func FromStore(s *store.Store) *Store { return &Store{s: s} }

func (b *Store) Get(ctx context.Context, key string) (blob.Object, error) {
	obj, err := b.s.ReadRaw(ctx, key)
	if err != nil {
		return blob.Object{}, fmt.Errorf("blobs: get %s: %w", key, err)
	}
	// ReadRaw already reports a miss as an empty object rather than an error,
	// which is the same convention blob.Blobs requires — so a miss passes
	// through as a zero Object with a nil error.
	return blob.Object{Content: obj.Content, ETag: obj.ETag}, nil
}

func (b *Store) Put(ctx context.Context, key, content string) error {
	err := b.s.WriteRaw(ctx, key, content, contentType, nil)
	if err != nil {
		return fmt.Errorf("blobs: put %s: %w", key, err)
	}
	return nil
}

func (b *Store) PutIfMatch(ctx context.Context, key, content, etag string) error {
	_, err := b.s.WriteRawIfMatch(ctx, key, content, contentType, nil, etag)
	return b.translate(key, err)
}

func (b *Store) PutIfAbsent(ctx context.Context, key, content string) error {
	_, err := b.s.WriteRawIfAbsent(ctx, key, content, contentType, nil)
	return b.translate(key, err)
}

// translate maps the store's precondition sentinel onto the auth package's, so
// callers can test for a lost claim with errors.Is against the interface's own
// error rather than reaching through to the implementation.
func (b *Store) translate(key string, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, store.ErrPrecondition):
		return blob.ErrPrecondition
	default:
		return fmt.Errorf("blobs: conditional put %s: %w", key, err)
	}
}

func (b *Store) Delete(ctx context.Context, key string) error {
	err := b.s.DeleteRaw(ctx, key)
	if err != nil {
		return fmt.Errorf("blobs: delete %s: %w", key, err)
	}
	return nil
}

func (b *Store) List(ctx context.Context, prefix string) ([]string, error) {
	keys, err := b.s.ListPrefix(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("blobs: list %s: %w", prefix, err)
	}
	return keys, nil
}
