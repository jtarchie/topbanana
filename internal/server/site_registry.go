package server

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/store"
)

// siteRegistry is the in-memory index of every site in the bucket, rebuilt from
// one ListApps + per-slug ReadMeta sweep. It answers the three questions the
// hot paths ask without an S3 round-trip:
//
//   - routing: which slug owns a custom hostname (domainIndex), and is a slug a
//     real app (slugIndex) — the latter keeps autocert from asking Let's Encrypt
//     for a cert for a scanner-invented hostname and burning the 50/week
//     per-registered-domain rate limit;
//   - ownership: which email owns a slug (ownerIndex), so role-filtered listings
//     and authorization don't pay an S3 GET per app;
//   - privacy: whether a slug is marked private (privateIndex), consulted by the
//     subdomain proxy on every public hit.
//
// All four maps are guarded by one RWMutex and rebuilt together, so a reader
// never sees a half-updated index.
type siteRegistry struct {
	store *store.Store
	build *build.Service

	mu           sync.RWMutex
	domainIndex  map[string]string
	slugIndex    map[string]bool
	ownerIndex   map[string]string
	privateIndex map[string]bool
}

// newSiteRegistry returns an empty registry; call initialRebuildIndexes (or
// rebuildIndexes) to populate it from the bucket.
func newSiteRegistry(st *store.Store, b *build.Service) *siteRegistry {
	return &siteRegistry{
		store:        st,
		build:        b,
		domainIndex:  map[string]string{},
		slugIndex:    map[string]bool{},
		ownerIndex:   map[string]string{},
		privateIndex: map[string]bool{},
	}
}

// rebuildIndexes scans all sites and rebuilds all four indexes (domain → slug,
// slug existence, owner, privacy) in one sweep. Called after any settings save
// that changes Domains, ownership, or privacy. Returns an error so the initial
// startup rebuild can retry; runtime callers (settings handlers) just log and
// continue — a stale index there only delays the next refresh.
func (r *siteRegistry) rebuildIndexes(ctx context.Context) error {
	apps, err := r.store.ListApps(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	idx := make(map[string]string, len(apps))
	slugs := make(map[string]bool, len(apps))
	owners := make(map[string]string, len(apps))
	privates := make(map[string]bool, len(apps))
	for _, slug := range apps {
		slugs[slug] = true
		meta := r.build.ReadMeta(ctx, slug)
		if meta.OwnerID != "" {
			owners[slug] = meta.OwnerID
		}
		if meta.Private {
			privates[slug] = true
		}
		for _, d := range meta.Domains {
			if existing, dup := idx[d]; dup && existing != slug {
				slog.Warn("site_index.duplicate", "domain", d, "kept", existing, "dropped", slug)
				continue
			}
			idx[d] = slug
		}
	}
	r.mu.Lock()
	r.domainIndex = idx
	r.slugIndex = slugs
	r.ownerIndex = owners
	r.privateIndex = privates
	r.mu.Unlock()
	slog.Info("site_index.rebuilt", "domains", len(idx), "slugs", len(slugs), "owners", len(owners), "private", len(privates))
	return nil
}

// ownerOf returns the owner email recorded in the index for a slug, or
// the empty string if the slug is unowned (pre-migration data). Cheap —
// one in-memory map lookup. The caller is expected to handle "":
// authorizeSlug treats it as "only super admin can access."
func (r *siteRegistry) ownerOf(slug string) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ownerIndex[slug]
}

// isPrivate reports whether the slug is marked private. Consulted by the
// subdomain dispatcher on every public-facing hit so we can gate the
// proxy without an extra S3 round-trip per request.
func (r *siteRegistry) isPrivate(slug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.privateIndex[slug]
}

// setOwner refreshes a single slug's owner without rebuilding the whole
// index. Called from buildHandler (after a new app is created) and from
// the transfer handler.
func (r *siteRegistry) setOwner(slug, owner string) {
	r.mu.Lock()
	if r.ownerIndex == nil {
		r.ownerIndex = map[string]string{}
	}
	r.ownerIndex[slug] = owner
	r.mu.Unlock()
}

// countAppsFor returns the number of slugs the given email owns according
// to the in-memory ownerIndex. Used by the quota check on /build and by
// the over-quota banner on /apps. Empty email returns 0.
func (r *siteRegistry) countAppsFor(email string) int {
	if email == "" {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	count := 0
	for _, owner := range r.ownerIndex {
		if owner == email {
			count++
		}
	}
	return count
}

// markSlug records a freshly-created slug so HostAllowed accepts it
// immediately, without waiting for the next ListApps rebuild. Called from
// buildHandler the moment a build is kicked off — the slug folder may not
// exist in S3 yet, but the user is already redirected to its URL and we want
// the first TLS handshake to succeed.
func (r *siteRegistry) markSlug(slug string) {
	r.mu.Lock()
	if r.slugIndex == nil {
		r.slugIndex = map[string]bool{}
	}
	r.slugIndex[slug] = true
	r.mu.Unlock()
}

// slugExists reports whether slug names a real app in our index. Used by
// HostAllowed to refuse ACME issuance for scanner-invented hostnames.
func (r *siteRegistry) slugExists(slug string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.slugIndex[slug]
}

// initialRebuildIndexes retries the first rebuild a few times. If S3 is
// briefly unreachable at boot and we silently start with an empty index, every
// custom-domain ACME validation fails closed (HostPolicy denies unknown hosts)
// until somebody saves settings — that's a long, silent outage. Keep retrying
// for ~10s; if the bucket genuinely is dead, panic so the platform restarts us.
func (r *siteRegistry) initialRebuildIndexes(ctx context.Context) {
	var lastErr error
	for i := range 5 {
		if i > 0 {
			time.Sleep(2 * time.Second)
		}
		err := r.rebuildIndexes(ctx)
		if err == nil {
			return
		}
		lastErr = err
		slog.Warn("site_index.startup_retry", "attempt", i+1, "err", err)
	}
	panic(fmt.Errorf("initial site index rebuild failed after retries: %w", lastErr))
}

// rebuildIndexesLogging is the post-startup callsite: rebuild, log on
// failure, keep serving. The old index stays in place if the rebuild errored.
func (r *siteRegistry) rebuildIndexesLogging(ctx context.Context) {
	err := r.rebuildIndexes(ctx)
	if err != nil {
		slog.Warn("site_index.refresh_failed", "err", err)
	}
}

// lookupCustomDomain returns the slug that owns host, if any.
func (r *siteRegistry) lookupCustomDomain(host string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	slug, ok := r.domainIndex[host]
	return slug, ok
}
