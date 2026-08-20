package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

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

// writeCollaborators persists a collaborator list to the sidecar and refreshes
// the in-memory index. Both are required: the sidecar is the truth an index
// rebuild reads back, and the index is what every access gate actually
// consults, so a write that updates only one of them either loses the grant on
// restart or doesn't take effect until the next sweep.
func (s *Server) writeCollaborators(c *echo.Context, slug string, meta build.SiteMeta, list []string) error {
	meta.Collaborators = normalizeCollaborators(list, meta.OwnerID)
	err := s.build.WriteMeta(c.Request().Context(), slug, meta)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "write meta", err)
	}
	s.registry.setCollaborators(slug, meta.Collaborators)
	return nil
}

// addCollaboratorHandler grants an existing user full working access to a
// site. Gated by requireSlugOwner upstream, so the caller is the owner or a
// super admin.
//
// The recipient must already have an account: issuing an invite here would let
// any site owner create users on the instance, which is a super-admin-only
// power (auth.InviteStore is reached from the admin surface alone). An unknown
// email is a 404 with the same wording the transfer form uses.
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
		return echo.NewHTTPError(http.StatusBadRequest, "that account is disabled")
	}

	meta := s.build.ReadMeta(ctx, slug)
	if recipient.Email == auth.NormalizeEmail(meta.OwnerID) {
		return echo.NewHTTPError(http.StatusBadRequest, "that user already owns this site")
	}
	// Idempotent: re-adding someone is a no-op with the same flash, not an
	// error. Two owners' tabs racing on the same email shouldn't 400.
	for _, existing := range meta.Collaborators {
		if existing == recipient.Email {
			return c.Redirect(http.StatusSeeOther, manageFlash(slug, recipient.Email+" already has access")) //nolint:wrapcheck
		}
	}
	if len(meta.Collaborators) >= maxCollaborators {
		return echo.NewHTTPError(http.StatusBadRequest,
			fmt.Sprintf("a site can have at most %d collaborators", maxCollaborators))
	}

	err = s.writeCollaborators(c, slug, meta, append(meta.Collaborators, recipient.Email))
	if err != nil {
		return err
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

	meta := s.build.ReadMeta(c.Request().Context(), slug)
	kept := make([]string, 0, len(meta.Collaborators))
	found := false
	for _, existing := range meta.Collaborators {
		if existing == target {
			found = true
			continue
		}
		kept = append(kept, existing)
	}
	if !found {
		return c.Redirect(http.StatusSeeOther, manageFlash(slug, target+" does not have access")) //nolint:wrapcheck
	}

	err = s.writeCollaborators(c, slug, meta, kept)
	if err != nil {
		return err
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
	slugs, err := s.store.ListApps(ctx)
	if err != nil {
		return 0, fmt.Errorf("list apps: %w", err)
	}
	count := 0
	var firstErr error
	for _, slug := range slugs {
		meta := s.build.ReadMeta(ctx, slug)
		kept := make([]string, 0, len(meta.Collaborators))
		for _, existing := range meta.Collaborators {
			if existing != email {
				kept = append(kept, existing)
			}
		}
		if len(kept) == len(meta.Collaborators) {
			continue
		}
		meta.Collaborators = normalizeCollaborators(kept, meta.OwnerID)
		werr := s.build.WriteMeta(ctx, slug, meta)
		if werr != nil {
			if firstErr == nil {
				firstErr = werr
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
