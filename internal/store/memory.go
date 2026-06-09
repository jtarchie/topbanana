package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// memoryBackend is an in-process objectBackend backed by a map, used by tests
// so the whole storage layer (and everything layered on it — editrec, snapshot,
// portable, build, auth) runs deterministically under `go test` without a live
// Minio. It deliberately stores opaque bytes only: Store still runs compression,
// path validation, and metadata encoding on top, so a test against this backend
// exercises the same contracts as one against S3.
//
// It is safe for concurrent use — the concurrency tests and the state-style
// conformance suite drive it from many goroutines.
type memoryBackend struct {
	mu      sync.RWMutex
	objects map[string]*memObject
	nextTag uint64
}

type memObject struct {
	body         []byte
	etag         string
	contentType  string
	metadata     map[string]string
	lastModified time.Time
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{objects: map[string]*memObject{}}
}

// tagLocked returns the next monotonic ETag. Caller holds b.mu.
func (b *memoryBackend) tagLocked() string {
	b.nextTag++
	return strconv.FormatUint(b.nextTag, 10)
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// storeMetadata clones metadata and lowercases its keys, mirroring how S3
// normalizes x-amz-meta-* header keys at rest — so ReadRaw/Read return the same
// key casing whether the backend is S3 or this map.
func storeMetadata(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[strings.ToLower(k)] = v
	}
	return out
}

func (b *memoryBackend) put(_ context.Context, key string, body []byte, contentType string, metadata map[string]string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	etag := b.tagLocked()
	b.objects[key] = &memObject{
		body:         cloneBytes(body),
		etag:         etag,
		contentType:  contentType,
		metadata:     storeMetadata(metadata),
		lastModified: time.Now().UTC(),
	}
	return etag, nil
}

func (b *memoryBackend) get(_ context.Context, key string) (*rawObject, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	obj, ok := b.objects[key]
	if !ok {
		return nil, nil //nolint:nilnil // a clean miss, mirroring s3Backend.get.
	}
	return &rawObject{
		body:        cloneBytes(obj.body),
		etag:        obj.etag,
		contentType: obj.contentType,
		metadata:    cloneMetadata(obj.metadata),
	}, nil
}

func (b *memoryBackend) list(_ context.Context, prefix string) ([]objectInfo, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	infos := make([]objectInfo, 0)
	for key, obj := range b.objects {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		infos = append(infos, objectInfo{key: key, size: int64(len(obj.body)), lastModified: obj.lastModified})
	}
	return infos, nil
}

func (b *memoryBackend) listApps(_ context.Context) ([]string, error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	seen := map[string]bool{}
	apps := make([]string, 0)
	for key := range b.objects {
		i := strings.IndexByte(key, '/')
		if i <= 0 {
			continue // top-level object with no "directory"; S3 wouldn't list it as a common prefix.
		}
		top := key[:i]
		if seen[top] {
			continue
		}
		seen[top] = true
		apps = append(apps, top)
	}
	return apps, nil
}

func (b *memoryBackend) remove(_ context.Context, key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	return nil
}

func (b *memoryBackend) copyObject(_ context.Context, srcKey, dstKey string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	src, ok := b.objects[srcKey]
	if !ok {
		return fmt.Errorf("failed to copy %s to %s: source not found", srcKey, dstKey)
	}
	b.objects[dstKey] = &memObject{
		body:         cloneBytes(src.body),
		etag:         b.tagLocked(),
		contentType:  src.contentType,
		metadata:     cloneMetadata(src.metadata),
		lastModified: time.Now().UTC(),
	}
	return nil
}

func (b *memoryBackend) replaceMeta(_ context.Context, key, contentType string, metadata map[string]string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	obj, ok := b.objects[key]
	if !ok {
		return fmt.Errorf("failed to update metadata on %s: not found", key)
	}
	obj.metadata = storeMetadata(metadata)
	if contentType != "" {
		obj.contentType = contentType
	}
	obj.etag = b.tagLocked()
	obj.lastModified = time.Now().UTC()
	return nil
}

func (b *memoryBackend) ensureBucket(_ context.Context) error { return nil }
