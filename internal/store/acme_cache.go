package store

import (
	"context"

	"golang.org/x/crypto/acme/autocert"
)

// ACMECache adapts Store to the autocert.Cache interface so Let's Encrypt
// account keys, issued certs, and short-lived challenge tokens persist in S3
// instead of an ephemeral container filesystem. Keys are namespaced under
// Prefix (default "_acme/") which sits outside the slug space — ListApps
// excludes leading-underscore prefixes, so it can't collide with a real app.
type ACMECache struct {
	Store  *Store
	Prefix string
}

// NewACMECache returns a Cache backed by store with the given key prefix. An
// empty prefix is allowed but discouraged; the constructor enforces a trailing
// slash if one is missing so callers don't have to think about it.
func NewACMECache(store *Store, prefix string) *ACMECache {
	if prefix != "" && prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	return &ACMECache{Store: store, Prefix: prefix}
}

// Get returns the cached blob for key. autocert.ErrCacheMiss is required when
// the key is absent — ReadRaw signals absence by returning an S3Object with
// empty Content (and a nil error), so we translate that to the sentinel here.
// Cached ACME values are never legitimately empty (account keys, DER blobs,
// challenge tokens all have content), so the heuristic is safe.
func (c *ACMECache) Get(ctx context.Context, key string) ([]byte, error) {
	obj, err := c.Store.ReadRaw(ctx, c.Prefix+key)
	if err != nil {
		return nil, err
	}
	if obj.Content == "" {
		return nil, autocert.ErrCacheMiss
	}
	return []byte(obj.Content), nil
}

func (c *ACMECache) Put(ctx context.Context, key string, data []byte) error {
	return c.Store.WriteRaw(ctx, c.Prefix+key, string(data), "application/octet-stream", nil)
}

func (c *ACMECache) Delete(ctx context.Context, key string) error {
	return c.Store.DeleteRaw(ctx, c.Prefix+key)
}
