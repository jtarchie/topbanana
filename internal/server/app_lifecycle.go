package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/build"
)

// This file owns app deletion and ownership/domain mutation: deleteApp and its
// owner-scoped cascades (used by account/user deletion), the settings delete
// handler, and the custom-domain parsing/diffing helpers. Extracted from
// server.go.

// newlyAddedDomains returns the hosts in next that weren't in prev. Both
// lists come from parseDomains so they're already normalized + lowercased.
func newlyAddedDomains(prev, next []string) []string {
	seen := make(map[string]bool, len(prev))
	for _, h := range prev {
		seen[h] = true
	}
	added := make([]string, 0, len(next))
	for _, h := range next {
		if !seen[h] {
			added = append(added, h)
		}
	}
	return added
}

// DeleteAppResult reports what deleteApp removed, for the caller's audit log.
type DeleteAppResult struct {
	FilesDeleted     int
	SnapshotsDeleted int
}

// deleteApp permanently removes one app's content: every site file, every
// snapshot, and its in-memory build/event state. It deliberately does NOT
// rebuild the domain index or log — callers own that, so a single delete logs
// once and a multi-app cascade (account / user deletion) rebuilds the index
// once after the last app. Returns the count of files and snapshots removed
// for the caller's audit log. Idempotent: re-running on an already-emptied
// slug lists zero files and is a no-op, which is what makes the cascade
// retry-safe.
func (s *Server) deleteApp(ctx context.Context, slug string) (DeleteAppResult, error) {
	paths, err := s.store.List(ctx, slug)
	if err != nil {
		return DeleteAppResult{}, fmt.Errorf("list files: %w", err)
	}
	for _, p := range paths {
		derr := s.store.Delete(ctx, slug, p)
		if derr != nil {
			return DeleteAppResult{}, fmt.Errorf("delete file: %w", derr)
		}
	}

	snaps := 0
	if s.snapshot != nil {
		snapList, lerr := s.snapshot.List(ctx, slug)
		if lerr != nil {
			return DeleteAppResult{}, fmt.Errorf("list snapshots: %w", lerr)
		}
		for _, sn := range snapList {
			derr := s.snapshot.Delete(ctx, slug, sn.Key)
			if derr != nil {
				return DeleteAppResult{}, fmt.Errorf("delete snapshot: %w", derr)
			}
		}
		snaps = len(snapList)
	}

	s.events.Forget(slug)
	return DeleteAppResult{FilesDeleted: len(paths), SnapshotsDeleted: snaps}, nil
}

// deleteAppsOwnedBy cascades deleteApp over every site owned by email. It uses
// ListApps + ReadMeta as the authoritative owner source rather than the
// in-memory ownerIndex, which can lag a just-built site. It collects the first
// error but keeps going so one wedged slug doesn't strand the rest, and leaves
// the single domain-index rebuild to the caller. An empty email is a no-op —
// it must never match the empty OwnerID of a pre-migration, super-admin-only
// site. Returns the number of apps actually removed.
func (s *Server) deleteAppsOwnedBy(ctx context.Context, email string) (int, error) {
	email = auth.NormalizeEmail(email)
	if email == "" {
		return 0, nil
	}
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		meta := s.build.ReadMeta(ctx, slug)
		if meta.OwnerID != email {
			continue
		}
		res, derr := s.deleteApp(ctx, slug)
		if derr != nil {
			if firstErr == nil {
				firstErr = derr
			}
			continue
		}
		slog.Info("app.delete", "slug", slug, "files", res.FilesDeleted, "snapshots", res.SnapshotsDeleted, "reason", "owner_delete")
		count++
	}
	return count, firstErr
}

// reassignAppsOwnedBy transfers every site owned by `from` to `to`, updating
// both the persisted SiteMeta.OwnerID and the in-memory owner index — the same
// per-site move transferAppHandler does, fanned across the whole owned set.
// Used when a super admin deletes a user but wants to preserve their sites
// under a new owner. Authoritative ListApps+ReadMeta scan; empty `from` is a
// no-op so it can't sweep pre-migration empty-owner sites. Collects the first
// error but keeps going, and leaves the single index rebuild to the caller.
// Returns the number of apps reassigned.
func (s *Server) reassignAppsOwnedBy(ctx context.Context, from, to string) (int, error) {
	from = auth.NormalizeEmail(from)
	to = auth.NormalizeEmail(to)
	if from == "" {
		return 0, nil
	}
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		meta := s.build.ReadMeta(ctx, slug)
		if meta.OwnerID != from {
			continue
		}
		meta.OwnerID = to
		werr := s.build.WriteMeta(ctx, slug, meta)
		if werr != nil {
			if firstErr == nil {
				firstErr = werr
			}
			continue
		}
		s.registry.setOwner(slug, to)
		slog.Info("app.transfer", "slug", slug, "from", from, "to", to, "reason", "owner_delete")
		count++
	}
	return count, firstErr
}

// settingsDeleteHandler permanently removes an app: all site files, all
// snapshots, the in-memory build status, and any custom-domain mapping. The
// caller must POST `confirm` equal to the slug — the typed-slug guard is the
// only safety check.
func (s *sitesController) settingsDeleteHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	if c.FormValue("confirm") != slug {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match slug")
	}

	ctx := c.Request().Context()
	res, err := s.deleteApp(ctx, slug)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete app", err)
	}
	s.registry.rebuildIndexesLogging(ctx)

	slog.Info("app.delete", "slug", slug, "files", res.FilesDeleted, "snapshots", res.SnapshotsDeleted)
	return c.Redirect(http.StatusSeeOther, "/apps?flash="+urlEscape("Deleted "+slug)) //nolint:wrapcheck
}

// parseDomains splits the settings-form textarea into a deduped, normalized
// list of hostnames. Rejects entries that collide with the main app domain
// (or its subdomains) and ones already claimed by another slug.
func (s *Server) parseDomains(raw, owningSlug string) ([]string, error) {
	seen := map[string]bool{}
	out := []string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, err := build.NormalizeDomain(line)
		if err != nil {
			return nil, fmt.Errorf("normalize domain: %w", err)
		}
		if host == s.domain || strings.HasSuffix(host, "."+s.domain) {
			return nil, fmt.Errorf("domain %q overlaps the main app domain", host)
		}
		if other, ok := s.registry.lookupCustomDomain(host); ok && other != owningSlug {
			return nil, fmt.Errorf("domain %q is already claimed by site %q", host, other)
		}
		if seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out, nil
}
