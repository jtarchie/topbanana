package state

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"
)

// Memory is an in-memory Store useful for tests and benchmarks. It enforces
// the same ETag CAS semantics as S3Store so handler-retry-on-conflict logic
// can be exercised without S3.
type Memory struct {
	mu  sync.Mutex
	gen atomic.Uint64
	dbs map[string]*memoryEntry
}

type memoryEntry struct {
	data map[string]any
	etag string
}

func NewMemory() *Memory {
	return &Memory{dbs: map[string]*memoryEntry{}}
}

func (m *Memory) Load(_ context.Context, slug string) (*Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.dbs[slug]
	if !ok {
		return NewSnapshot(), nil
	}
	// Hand back a copy so the caller's mutations don't leak back into the
	// store until Save is called.
	cp := make(map[string]any, len(e.data))
	for k, v := range e.data {
		cp[k] = v
	}
	return &Snapshot{Data: cp, ETag: e.etag}, nil
}

func (m *Memory) Save(_ context.Context, slug string, snap *Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.dbs[slug]
	switch {
	case !exists && snap.ETag != "":
		// We were told there is a version to overwrite, but nothing exists.
		return ErrConflict
	case exists && cur.etag != snap.ETag:
		return ErrConflict
	}
	next := m.nextETag()
	cp := make(map[string]any, len(snap.Data))
	for k, v := range snap.Data {
		cp[k] = v
	}
	m.dbs[slug] = &memoryEntry{data: cp, etag: next}
	snap.ETag = next
	snap.Dirty = false
	return nil
}

func (m *Memory) nextETag() string {
	return `"` + strconv.FormatUint(m.gen.Add(1), 10) + `"`
}
