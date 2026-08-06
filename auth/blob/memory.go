package blob

import (
	"context"
	"strconv"
	"strings"
	"sync"
)

// Memory is a complete in-process Blobs implementation. It is not a stub: it
// enforces the same If-Match / If-None-Match semantics a real object store
// does, because a double that is looser than production hides exactly the bugs
// this contract exists to prevent — a single-use claim that admits two winners
// looks fine against a permissive fake.
//
// Use it for tests, for single-process deployments, and as the reference when
// writing an adapter for a real store. Safe for concurrent use.
type Memory struct {
	mu      sync.Mutex
	objects map[string]memObject
	nextTag uint64
}

type memObject struct {
	content string
	etag    string
}

func NewMemory() *Memory {
	return &Memory{objects: map[string]memObject{}}
}

// tagLocked returns the next version tag. Caller holds m.mu.
func (m *Memory) tagLocked() string {
	m.nextTag++
	return strconv.FormatUint(m.nextTag, 10)
}

func (m *Memory) Get(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	obj, ok := m.objects[key]
	if !ok {
		// A miss is an ordinary answer, not a fault. See Blobs.Get.
		return Object{}, nil
	}
	return Object{Content: obj.content, ETag: obj.etag}, nil
}

func (m *Memory) Put(_ context.Context, key, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[key] = memObject{content: content, etag: m.tagLocked()}
	return nil
}

func (m *Memory) PutIfMatch(_ context.Context, key, content, etag string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cur, exists := m.objects[key]
	// The existence check shares an expression with the field read so
	// short-circuiting guarantees cur is populated. A missing key loses here,
	// matching S3 and Minio, which answer 404 rather than 412 — callers get one
	// "you lost" signal either way.
	if !exists || cur.etag != etag {
		return ErrPrecondition
	}
	m.objects[key] = memObject{content: content, etag: m.tagLocked()}
	return nil
}

func (m *Memory) PutIfAbsent(_ context.Context, key, content string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.objects[key]; exists {
		return ErrPrecondition
	}
	m.objects[key] = memObject{content: content, etag: m.tagLocked()}
	return nil
}

func (m *Memory) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// Deleting an absent key is not an error.
	delete(m.objects, key)
	return nil
}

func (m *Memory) List(_ context.Context, prefix string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	keys := make([]string, 0, len(m.objects))
	for k := range m.objects {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}
