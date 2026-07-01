// Package photowall is the domain model for the event-photo-wall feature:
// visitor-uploaded photos held for owner approval, then shown on a rotating
// full-screen display. It owns the typed metadata row, the key/path schema,
// the (single) legal status transition, and the upload guardrails (allowed
// image types, pending cap, per-IP rate limiter).
//
// It deliberately holds no HTTP or storage wiring — the server package builds
// the handlers on top of these pure helpers, keeping the domain rules testable
// and satisfying the depguard rule that nothing outside cmd/ imports
// internal/server.
package photowall

import (
	"fmt"
	"sort"
	"strings"

	"github.com/jtarchie/topbanana/internal/store"
)

// Status values a photo row can hold. There is no auto-approve mode: every
// upload lands pending and only the owner can move it to approved.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
)

const (
	// metaPrefix namespaces the per-photo metadata rows inside the site's KV
	// blob (`{slug}/_state/data.json`). Object-valued so the existing manage
	// submissions machinery (collectSubmissions/deleteSubmissionKey) sees them.
	metaPrefix = "photo:"

	// SeqKey is the scalar monotonic counter that mints photo ids, mirroring
	// the guestbook's `kv.incr("seq")`.
	SeqKey = "photo_seq"

	// ApprovedSubdir is the public, proxy-served location approved photo bytes
	// are Copied to on approval. Under assets/ so the static proxy and the CDN
	// serve them unchanged.
	ApprovedSubdir = "assets/photowall/"
)

// DefaultPendingCap bounds how many un-approved photos may sit in the queue at
// once. An open QR upload link is a spam vector; once the queue is full further
// uploads are refused until the owner clears the backlog.
const DefaultPendingCap = 100

// contentTypeExt maps the sniffed image content types the wall accepts to the
// extension the bytes are stored under. SVG is intentionally excluded — a photo
// wall takes photographs, and SVG is a script-bearing vector format we don't
// want visitors posting unmoderated.
var contentTypeExt = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/gif":  ".gif",
	"image/webp": ".webp",
}

// Photo is the typed view of one `photo:{id}` metadata row.
type Photo struct {
	ID     string // zero-padded 8-digit decimal, e.g. "00000042"
	Status string // StatusPending | StatusApproved
	Ext    string // stored extension incl. dot, e.g. ".jpg"
	Asset  string // approved bytes path, set only once approved
	TS     int64  // upload time, unix millis
}

// MetaKey returns the KV key for a photo id: "photo:00000042".
func MetaKey(id string) string { return metaPrefix + id }

// IsPhotoKey reports whether a KV key is a photo metadata row.
func IsPhotoKey(key string) bool { return strings.HasPrefix(key, metaPrefix) }

// ParseID extracts the id from a photo KV key. ok is false when the key isn't a
// photo row or carries an empty id.
func ParseID(key string) (string, bool) {
	if !IsPhotoKey(key) {
		return "", false
	}
	id := strings.TrimPrefix(key, metaPrefix)
	if id == "" {
		return "", false
	}
	return id, true
}

// FormatID zero-pads a sequence number to 8 digits so lexicographic key order
// matches insertion order (the sort collectSubmissions relies on).
func FormatID(seq int64) string { return fmt.Sprintf("%08d", seq) }

// PendingPath is the reserved-prefix path un-approved bytes are written to,
// e.g. "_pending/00000042.jpg". Aliases store.PendingDir so the reserved
// keyspace has a single registry entry (see internal/store/keyspace.go).
func PendingPath(id, ext string) string { return store.PendingDir + id + ext }

// ApprovedPath is the public path approved bytes are Copied to, e.g.
// "assets/photowall/00000042.jpg".
func ApprovedPath(id, ext string) string { return ApprovedSubdir + id + ext }

// ExtForContentType returns the stored extension for a sniffed image content
// type, and whether the type is an accepted photo type.
func ExtForContentType(contentType string) (string, bool) {
	ext, ok := contentTypeExt[contentType]
	return ext, ok
}

// AllowedExt reports whether ext (incl. leading dot) is one the wall stores.
func AllowedExt(ext string) bool {
	for _, e := range contentTypeExt {
		if e == ext {
			return true
		}
	}
	return false
}

// CanTransition enforces the one legal status edge: pending -> approved.
// Everything else (re-approve, un-approve, unknown status) is rejected so a
// stray double-submit can't corrupt a row.
func CanTransition(from, to string) bool {
	return from == StatusPending && to == StatusApproved
}

// FromMeta decodes a KV row (map[string]any, as JSON-decoded) into a Photo.
// Tolerant of encoding/json's float64 numbers for ts. ok is false when the row
// isn't a well-formed photo object.
func FromMeta(m map[string]any) (Photo, bool) {
	if m == nil {
		return Photo{}, false
	}
	id, _ := m["id"].(string)
	status, _ := m["status"].(string)
	if id == "" || (status != StatusPending && status != StatusApproved) {
		return Photo{}, false
	}
	ext, _ := m["ext"].(string)
	asset, _ := m["asset"].(string)
	p := Photo{ID: id, Status: status, Ext: ext, Asset: asset, TS: tsFromAny(m["ts"])}
	return p, true
}

// ToMeta encodes a Photo back to the KV row shape. Asset is omitted while the
// photo is pending so the row carries no stale path.
func (p Photo) ToMeta() map[string]any {
	m := map[string]any{
		"id":     p.ID,
		"status": p.Status,
		"ext":    p.Ext,
		"ts":     p.TS,
	}
	if p.Asset != "" {
		m["asset"] = p.Asset
	}
	return m
}

// Collect returns every photo row in a state snapshot with the given status,
// newest-first. Ids are zero-padded so a descending string sort on the id is
// insertion order reversed — the order both the approved display and the
// moderation queue want.
func Collect(data map[string]any, status string) []Photo {
	var out []Photo
	for key, v := range data {
		if !IsPhotoKey(key) {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		p, ok := FromMeta(m)
		if !ok || p.Status != status {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}

// CountPending tallies how many photo rows in a state snapshot are pending.
// Used to enforce DefaultPendingCap before accepting another upload.
func CountPending(data map[string]any) int {
	n := 0
	for key, v := range data {
		if !IsPhotoKey(key) {
			continue
		}
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if p, ok := FromMeta(m); ok && p.Status == StatusPending {
			n++
		}
	}
	return n
}

// tsFromAny coerces the numeric forms a ts value might take after a JSON
// round-trip (float64) or when set directly in Go (int64/int).
func tsFromAny(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}
