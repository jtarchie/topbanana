package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/auth"
	"github.com/jtarchie/topbanana/internal/build"
)

// This file owns collaboration: additional users with full working access to a
// site they don't own. The access predicate itself lives on the registry
// (canAccess) because every gate consults it on the hot path; what lives here
// is the policy around mutating the list and the two owner-only handlers.
//
// Deliberately flat: a collaborator can do everything the owner can except
// delete the site, transfer it, or change this list. One boolean keeps the
// authorization surface a boolean everywhere it's checked — web routes, the
// live-site edit toolbar, the private-site proxy, and the MCP tools — rather
// than a permission lookup repeated five times.

// maxCollaborators bounds the list. The sidecar is read once per slug on every
// index rebuild, so an unbounded list is a per-site tax on a global sweep; ten
// is well past what a site with one accountable owner needs.
const maxCollaborators = 10

// normalizeCollaborators canonicalizes a collaborator list for storage: emails
// normalized, blanks and duplicates dropped, and the owner removed (they
// already have access, and leaving them in would make removing a collaborator
// able to look like demoting the owner). Returns nil for an empty result so
// the sidecar omits the field entirely.
func normalizeCollaborators(list []string, owner string) []string {
	owner = auth.NormalizeEmail(owner)
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, raw := range list {
		email := auth.NormalizeEmail(raw)
		if email == "" || email == owner || seen[email] {
			continue
		}
		seen[email] = true
		out = append(out, email)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// metaGrantsAccess is canAccess against a sidecar the caller already holds,
// for the paths that read metadata authoritatively rather than through the
// in-memory index (the /apps listing, the owner-scoped sweeps). Kept beside
// canAccess so the two definitions of access can't drift.
func metaGrantsAccess(meta build.SiteMeta, email string) bool {
	if email == "" {
		return false
	}
	if meta.OwnerID == email {
		return true
	}
	for _, c := range meta.Collaborators {
		if c == email {
			return true
		}
	}
	return false
}

// Outcomes a collaborator-list mutation can report back from inside the
// compare-and-set closure. They're sentinels rather than HTTP errors because
// the closure runs against whatever the sidecar says at write time, which may
// differ from what the handler read a moment earlier — the decision has to be
// made where the authoritative value is.
var (
	errAlreadyOwner  = errors.New("that user already owns this site")
	errAlreadyShared = errors.New("already has access")
	errNotShared     = errors.New("does not have access")
	errTooManyShares = errors.New("collaborator limit reached")
)

// updateCollaborators is the one write path for the list. It goes through
// build.UpdateMeta — a compare-and-set read-modify-write — rather than
// ReadMeta + WriteMeta, because the sidecar now has more than one writer with
// access to it: an owner sharing a site while a collaborator saves settings
// would otherwise lose whichever change was read first, and a lost *removal*
// silently restores access. UpdateMeta also surfaces a failed read instead of
// persisting ReadMeta's zero value, which would blank OwnerID and orphan the
// site.
//
// mutate reports its outcome through the returned sentinel; it must be a pure
// function of the meta it's handed, since a CAS retry calls it again.
func (s *Server) updateCollaborators(
	ctx context.Context,
	slug string,
	mutate func(meta *build.SiteMeta) error,
) (build.SiteMeta, error, error) {
	var outcome error
	meta, err := s.build.UpdateMeta(ctx, slug, func(m *build.SiteMeta) {
		outcome = mutate(m)
		m.Collaborators = normalizeCollaborators(m.Collaborators, m.OwnerID)
	})
	if err != nil {
		return build.SiteMeta{}, nil, httpErr(http.StatusInternalServerError, "update site meta", err)
	}
	s.registry.setCollaborators(slug, meta.Collaborators)
	return meta, outcome, nil
}

// addCollaboratorHandler grants an existing user full working access to a
// site. Gated by requireSlugOwner upstream, so the caller is the owner or a
// super admin.
//
// The recipient must already have an account: issuing an invite here would let
// any site owner create users on the instance, which is a super-admin-only
// power (auth.InviteStore is reached from the admin surface alone). An unknown
// address and a disabled one answer identically — sharing is a routine action
// offered to every owner, and distinguishable responses would turn each of
// them into an oracle for who has an account on an invite-only instance. A
// successful share still confirms the address exists, which is inherent to the
// feature; what this removes is the ability to probe addresses you can't share
// with.
func (s *sitesController) addCollaboratorHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	target := auth.NormalizeEmail(c.FormValue("collaborator_email"))
	if target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "collaborator_email required")
	}

	ctx := c.Request().Context()
	recipient, err := s.auth.Users.Load(ctx, target)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return echo.NewHTTPError(http.StatusNotFound, "no user with that email")
		}
		return httpErr(http.StatusInternalServerError, "load collaborator", err)
	}
	if recipient.Disabled {
		return echo.NewHTTPError(http.StatusNotFound, "no user with that email")
	}

	_, outcome, err := s.updateCollaborators(ctx, slug, func(m *build.SiteMeta) error {
		if recipient.Email == auth.NormalizeEmail(m.OwnerID) {
			return errAlreadyOwner
		}
		for _, existing := range m.Collaborators {
			if existing == recipient.Email {
				return errAlreadyShared
			}
		}
		if len(m.Collaborators) >= maxCollaborators {
			return errTooManyShares
		}
		m.Collaborators = append(m.Collaborators, recipient.Email)
		return nil
	})
	if err != nil {
		return err
	}
	switch {
	case errors.Is(outcome, errAlreadyOwner):
		return echo.NewHTTPError(http.StatusBadRequest, "that user already owns this site")
	case errors.Is(outcome, errTooManyShares):
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("a site can have at most %d collaborators", maxCollaborators))
	case errors.Is(outcome, errAlreadyShared):
		// Idempotent: re-adding someone is a no-op with a flash, not an error.
		return c.Redirect(http.StatusSeeOther, manageFlash(slug, recipient.Email+" already has access")) //nolint:wrapcheck
	}

	slog.Info("app.collaborator.add", "slug", slug, "email", recipient.Email, "by", callerEmail(c))
	return c.Redirect(http.StatusSeeOther, manageFlash(slug, "Shared with "+recipient.Email)) //nolint:wrapcheck
}

// removeCollaboratorHandler revokes one collaborator's access. Takes the email
// verbatim from the form rather than re-loading the user record: the grant has
// to be removable even after the account is gone or disabled, and the stored
// list is already canonical.
func (s *sitesController) removeCollaboratorHandler(c *echo.Context) error {
	slug, err := slugParam(c)
	if err != nil {
		return err
	}
	target := auth.NormalizeEmail(c.FormValue("collaborator_email"))
	if target == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "collaborator_email required")
	}

	_, outcome, err := s.updateCollaborators(c.Request().Context(), slug, func(m *build.SiteMeta) error {
		kept := make([]string, 0, len(m.Collaborators))
		found := false
		for _, existing := range m.Collaborators {
			if existing == target {
				found = true
				continue
			}
			kept = append(kept, existing)
		}
		if !found {
			return errNotShared
		}
		m.Collaborators = kept
		return nil
	})
	if err != nil {
		return err
	}
	if errors.Is(outcome, errNotShared) {
		return c.Redirect(http.StatusSeeOther, manageFlash(slug, target+" does not have access")) //nolint:wrapcheck
	}

	slog.Info("app.collaborator.remove", "slug", slug, "email", target, "by", callerEmail(c))
	return c.Redirect(http.StatusSeeOther, manageFlash(slug, "Removed "+target)) //nolint:wrapcheck
}

// removeCollaboratorEverywhere strips one email from every site's collaborator
// list. Called when an account is deleted, alongside the owner-side cascade:
// without it a deleted address lingers in sidecars, and re-inviting the same
// address (emails are the identity here — see auth.User) silently restores
// access to sites nobody meant to re-share. Collects the first error but keeps
// going, and leaves the index rebuild to the caller like the other sweeps.
// Empty email is a no-op. Returns the number of sites changed.
func (s *Server) removeCollaboratorEverywhere(ctx context.Context, email string) (int, error) {
	email = auth.NormalizeEmail(email)
	if email == "" {
		return 0, nil
	}
	// Authoritative ListApps + ReadMeta rather than the in-memory collabIndex,
	// for the same reason deleteAppsOwnedBy scans: the index is per-process, so
	// another instance's just-added grant isn't in ours. The ReadMeta pass is
	// the cheap filter — only sites that actually name the address pay for a
	// compare-and-set write.
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		if !slices.Contains(s.build.ReadMeta(ctx, slug).Collaborators, email) {
			continue
		}
		meta, uerr := s.build.UpdateMeta(ctx, slug, func(m *build.SiteMeta) {
			kept := make([]string, 0, len(m.Collaborators))
			for _, existing := range m.Collaborators {
				if existing != email {
					kept = append(kept, existing)
				}
			}
			m.Collaborators = normalizeCollaborators(kept, m.OwnerID)
		})
		if uerr != nil {
			if firstErr == nil {
				firstErr = uerr
			}
			continue
		}
		s.registry.setCollaborators(slug, meta.Collaborators)
		slog.Info("app.collaborator.remove", "slug", slug, "email", email, "reason", "account_delete")
		count++
	}
	return count, firstErr
}

// manageFlash builds the post-action redirect back to the site's manage page.
func manageFlash(slug, msg string) string {
	return "/manage/" + slug + "?flash=" + urlEscape(msg)
}
