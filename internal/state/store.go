// Package state is the per-site key-value store backing the sandbox's `kv.*`
// API. The Store interface is deliberately narrow — Load and Save over a
// Snapshot — so we can swap implementations later (cache layer, bbolt, SQLite)
// without touching the sandbox or agent surfaces.
//
// The default implementation is S3Store, which loads and saves the whole
// per-site blob on every request with ETag-based optimistic concurrency. Slow
// by web standards (~30-100 ms per dynamic request), simple by everything-else
// standards.
package state

import (
	"context"
	"errors"
)

// Snapshot is a per-request working copy of a slug's KV state. Handlers mutate
// Data in memory; if Dirty is true at the end, the host calls Store.Save to
// persist it. ETag is the precondition for the next save; an empty ETag means
// "no version exists yet" and Save will use If-None-Match: * to avoid trampling
// a concurrent creator.
type Snapshot struct {
	Data  map[string]any
	ETag  string
	Dirty bool
}

// NewSnapshot returns an empty Snapshot suitable for first-write scenarios.
func NewSnapshot() *Snapshot {
	return &Snapshot{Data: map[string]any{}}
}

// ErrConflict is returned by Save when the underlying storage rejects the
// write because the ETag precondition failed. Callers should reload and retry
// the handler.
var ErrConflict = errors.New("state: cas conflict")

// Store is the per-site state backend. Implementations must be safe for
// concurrent use across slugs and serialize concurrent writes to the same
// slug via the ETag precondition surfaced through Snapshot.
type Store interface {
	// Load returns the current Snapshot for slug. A missing slug returns an
	// empty Snapshot with an empty ETag — not an error.
	Load(ctx context.Context, slug string) (*Snapshot, error)

	// Save writes snap.Data back as the new state for slug, conditional on
	// snap.ETag matching the current stored version. Returns ErrConflict on
	// precondition failure; snap.ETag is updated to the new version on success.
	Save(ctx context.Context, slug string, snap *Snapshot) error
}
