package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// This file owns the per-site metadata sidecar: its on-disk shape (SiteMeta),
// the read/write/legacy-fallthrough I/O, the effective-template override, and
// custom-domain normalization. Persistence is really a storage concern that
// build happens to own; keeping it here makes that boundary explicit.

// MetaFile holds the per-site sidecar (template id, creation time, custom
// domains). Stored alongside the HTML files in the same S3 prefix so it
// travels with the site.
const MetaFile = ".topbanana.json"

// legacyMetaFiles are pre-rebrand sidecar names, newest first. ReadMeta falls
// through them in order when MetaFile is absent so sites created before a
// rebrand keep their template id, custom domains, and function flags:
// `.bloomhollow.json` predates the Top Banana rebrand, `.buildabear.json`
// predates the Bloomhollow rebrand. The next successful WriteMeta migrates the
// site to MetaFile.
var legacyMetaFiles = []string{".bloomhollow.json", ".buildabear.json"}

// SiteMeta is the per-site sidecar persisted at MetaFile.
//
// EnablesFunctions is a per-site override layered on top of the template's
// default — set when a user opts in to dynamic features from the settings page
// for a site whose template didn't ship with them. Always read through
// EffectiveTemplate so the override is honoured everywhere the template's
// bit is consulted.
//
// Domains are external hostnames (e.g. `example.com`, `www.example.com`)
// that resolve to this site. Lowercased + port-stripped on write; the server
// builds a reverse Host → slug index from them.
type SiteMeta struct {
	Template         string    `json:"template"`
	Created          time.Time `json:"created"`
	Domains          []string  `json:"domains,omitempty"`
	EnablesFunctions bool      `json:"enables_functions,omitempty"`
	// EnablesPublicAPI opts the site's /api/* routes out of the same-origin
	// check applied to state-changing requests. Off by default so a freshly
	// built site can't be drive-by-spammed from another origin. Turn on for
	// genuine public APIs (webhooks, public JSON endpoints).
	EnablesPublicAPI bool   `json:"enables_public_api,omitempty"`
	Title            string `json:"title,omitempty"`
	Description      string `json:"description,omitempty"`
	// OwnerID is the canonical email of the user that owns this app. Empty on
	// pre-multi-tenancy sites; the bootstrap migration assigns those to the
	// super admin on first start.
	OwnerID string `json:"owner_id,omitempty"`
	// Private hides the site from the public web. When true the subdomain
	// (and any custom domains) return 404 to everyone except the owner and
	// super admins.
	Private bool `json:"private,omitempty"`
	// Collaborators are additional users with full working access to this
	// site: everything the owner can do except delete it, transfer it, or
	// change this list. Ownership stays a scalar on purpose — OwnerID is the
	// single principal quota counts against, account deletion cascades from,
	// and transfer moves — so collaboration is additive and needs no
	// migration. Canonical emails, deduped, never containing OwnerID.
	Collaborators []string `json:"collaborators,omitempty"`
}

// EffectiveTemplate returns the template a build/edit/route lookup should use,
// OR-ing the per-site EnablesFunctions override on top of the template's
// default. Returns a shallow copy when the override flips the bit on so
// callers can never mutate the package-level template registry.
func EffectiveTemplate(meta SiteMeta) *templates.SiteTemplate {
	base := templates.Get(meta.Template)
	if base == nil || base.EnablesFunctions || !meta.EnablesFunctions {
		return base
	}
	out := *base
	out.EnablesFunctions = true
	return &out
}

// WriteMeta persists the per-site sidecar.
func (svc *Service) WriteMeta(ctx context.Context, slug string, meta SiteMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode site meta: %w", err)
	}
	err = svc.store.Write(ctx, slug, MetaFile, string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("write site meta: %w", err)
	}
	return nil
}

// NormalizeDomain lowercases and strips an optional port from a user-entered
// host. Returns an error for empty input or anything that isn't a plausible
// hostname (catches obvious typos like trailing slashes or schemes).
func NormalizeDomain(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndex(h, ":"); i != -1 {
		h = h[:i]
	}
	if h == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(h, "/ \t\r\n") || strings.Contains(h, "://") {
		return "", fmt.Errorf("invalid domain %q", raw)
	}
	if !strings.Contains(h, ".") {
		return "", fmt.Errorf("domain %q must contain a dot", raw)
	}
	return h, nil
}

// metaUpdateAttempts bounds the compare-and-set retry loop. Contention here is
// human-scale (two people on one site's settings), so a couple of retries is
// already generous; the cap exists so a pathological loser can't spin.
const metaUpdateAttempts = 5

// ErrMetaConflict is returned when UpdateMeta lost the compare-and-set race
// metaUpdateAttempts times running. Distinct from a write failure: nothing is
// wrong, the caller just never got a clean window.
var ErrMetaConflict = errors.New("site meta: concurrent update, retry")

// UpdateMeta is the safe read-modify-write for the sidecar: it reads the
// current value, hands it to mutate, and stores the result only if nobody else
// wrote in between — retrying from a fresh read when they did.
//
// Every mutating caller must use this rather than ReadMeta + WriteMeta, for
// two reasons the pair can't give you:
//
//   - ReadMeta answers a zero SiteMeta when the read *fails*, which a plain
//     read-modify-write then persists: OwnerID gone (the site is orphaned and
//     its owner locked out), Domains gone, Private cleared. UpdateMeta returns
//     the read error instead of writing anything.
//   - A site now has more than one person who can change it. Last-writer-wins
//     silently drops whichever change was read first, and when that change was
//     a revoked collaborator, the revocation is undone.
//
// mutate may be called more than once and must be a pure function of the meta
// it is handed. Returns the stored value.
func (svc *Service) UpdateMeta(ctx context.Context, slug string, mutate func(*SiteMeta)) (SiteMeta, error) {
	var lastErr error
	for attempt := range metaUpdateAttempts {
		meta, etag, err := svc.readMetaForUpdate(ctx, slug, attempt > 0)
		if err != nil {
			return SiteMeta{}, err
		}
		mutate(&meta)
		body, err := json.Marshal(meta)
		if err != nil {
			return SiteMeta{}, fmt.Errorf("encode site meta: %w", err)
		}
		_, err = svc.store.WriteConditional(ctx, slug, MetaFile, string(body), "application/json", nil, etag)
		if err == nil {
			return meta, nil
		}
		if !errors.Is(err, store.ErrPrecondition) {
			return SiteMeta{}, fmt.Errorf("write site meta: %w", err)
		}
		lastErr = err
		slog.Info("site_meta.cas_retry", "slug", slug, "attempt", attempt+1)
	}
	return SiteMeta{}, fmt.Errorf("%w: %w", ErrMetaConflict, lastErr)
}

// readMetaForUpdate is ReadMeta's error-returning sibling: it reports a failed
// read instead of degrading to a zero value, and returns the ETag the write
// half has to match. An absent sidecar yields ("", zero meta) — an empty ETag
// means "must not exist" to WriteConditional, so a site whose sidecar is being
// created for the first time is still a one-winner race.
//
// Legacy sidecars are read for their content but deliberately contribute no
// ETag: the write always lands on MetaFile, which is a create.
func (svc *Service) readMetaForUpdate(ctx context.Context, slug string, fresh bool) (SiteMeta, string, error) {
	read := svc.store.Read
	if fresh {
		read = svc.store.ReadFresh
	}
	obj, err := read(ctx, slug, MetaFile)
	if err != nil {
		return SiteMeta{}, "", fmt.Errorf("read site meta: %w", err)
	}
	if obj.Content != "" {
		var m SiteMeta
		derr := json.Unmarshal([]byte(obj.Content), &m)
		if derr != nil {
			return SiteMeta{}, "", fmt.Errorf("decode site meta: %w", derr)
		}
		return m, obj.ETag, nil
	}
	for _, legacy := range legacyMetaFiles {
		lobj, lerr := svc.store.Read(ctx, slug, legacy)
		if lerr != nil {
			return SiteMeta{}, "", fmt.Errorf("read legacy site meta: %w", lerr)
		}
		if lobj.Content == "" {
			continue
		}
		var m SiteMeta
		derr := json.Unmarshal([]byte(lobj.Content), &m)
		if derr != nil {
			return SiteMeta{}, "", fmt.Errorf("decode legacy site meta: %w", derr)
		}
		return m, "", nil
	}
	return SiteMeta{}, "", nil
}

// ReadMeta returns the recorded sidecar for an existing site, or a zero value
// if the sidecar is missing (older sites pre-date templates). Falls through
// to legacyMetaFiles in order for sites created before a rebrand.
func (svc *Service) ReadMeta(ctx context.Context, slug string) SiteMeta {
	obj, err := svc.store.Read(ctx, slug, MetaFile)
	if err != nil {
		slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	for _, legacy := range legacyMetaFiles {
		if obj.Content != "" {
			break
		}
		obj, err = svc.store.Read(ctx, slug, legacy)
		if err != nil {
			slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
			return SiteMeta{}
		}
	}
	if obj.Content == "" {
		return SiteMeta{}
	}
	var m SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &m)
	if err != nil {
		slog.Warn("site_meta.decode_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	return m
}
