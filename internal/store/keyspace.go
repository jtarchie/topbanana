package store

// This file is the single registry of the bucket's reserved keyspace. Two
// tiers exist, and confusing them has already produced one dead dashboard row
// (a bucket-level sum over the in-slug _state/ dir always reported zero):
//
//   - Bucket-level prefixes sit at the top of the bucket, outside any slug.
//     ListApps hides every top-level prefix that starts with "_" (slugs cannot
//     start with an underscore per validateSlug), which is what keeps these
//     areas out of app listings and subdomain routing.
//
//   - In-slug paths live under `{slug}/...` and are reserved per site: the
//     static proxy refuses to serve them and the file explorer treats them as
//     platform-managed.
//
// Packages that own one of these areas (snapshot, editrec, state, portable)
// alias their local constant to the one here, so adding or renaming a reserved
// area is one edit plus the compiler pointing at every consumer. The one
// non-compiled copy is cmd/topbanana's --acme-cache-prefix kong default, which
// must be a struct-tag literal; it mirrors DefaultACMEPrefix.
const (
	// SnapshotsPrefix holds per-site version-history archives, keyed
	// `_snapshots/{slug}/...` (internal/snapshot).
	SnapshotsPrefix = "_snapshots/"

	// EditsPrefix holds per-edit build transcripts, keyed `_edits/{slug}/...`
	// (internal/editrec).
	EditsPrefix = "_edits/"

	// DefaultACMEPrefix is the default home of the autocert account key and
	// certificate cache (store.ACMECache); overridable via --acme-cache-prefix.
	DefaultACMEPrefix = "_acme/"

	// StateDir is the in-slug directory for persisted form/KV data:
	// `{slug}/_state/data.json` (internal/state). Unlike the prefixes above it
	// exists once per site, so any bucket-level aggregation must walk slugs.
	StateDir = "_state/"

	// PendingDir is the in-slug directory for un-approved photo-wall bytes:
	// `{slug}/_pending/{id}.jpg` (internal/photowall). Proxy-blocked like
	// StateDir so visitor uploads can't be viewed before the owner approves
	// them; approval Copies the bytes out to the public assets/ tree.
	PendingDir = "_pending/"
)
