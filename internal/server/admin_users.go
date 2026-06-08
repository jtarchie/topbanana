package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
	"github.com/jtarchie/topbanana/internal/model"
)

// adminUserRow is one row in the user table on /admin/users. ModelAuthor /
// ModelEditor / ModelUtility / ModelVision are the per-tier overrides; an
// empty string means "inherit the system default for this tier".
type adminUserRow struct {
	Email        string
	Role         string
	Disabled     bool
	Credentials  int
	Created      string
	IsSelf       bool
	MaxApps      int // 0 = uses default
	ModelAuthor  string
	ModelEditor  string
	ModelUtility string
	ModelVision  string
}

// adminInviteRow is one row in the pending-invites table on /admin/users.
type adminInviteRow struct {
	Token   string
	Email   string
	Role    string
	Expires string
	URL     string
}

// adminUsersData backs templates/admin_users.html.
type adminUsersData struct {
	Chrome
	Users           []adminUserRow
	Invites         []adminInviteRow
	Flash           string
	Error           string
	Roles           []string
	SuggestedModels []string
}

// suggestedModels feeds the <datalist> on the Quotas panel's per-tier
// model inputs so a super-admin gets autocomplete on the common cases
// instead of recalling exact provider/model strings (the page's biggest
// recall cost, surfaced by the impeccable critique). Not exhaustive
// and not enforced: the field stays free-text so admins can paste a
// model id this list doesn't know yet. Update by hand when new model
// generations ship.
var suggestedModels = []string{
	"anthropic/claude-opus-4-7",
	"anthropic/claude-sonnet-4-6",
	"anthropic/claude-haiku-4-5",
	"openai/gpt-4o",
	"openai/gpt-4o-mini",
	"openrouter/anthropic/claude-sonnet-4-6",
	"openrouter/openai/gpt-4o-mini",
	"lmstudio/google/gemma-4-12b",
}

// adminUsersHandler renders the super-admin user/invite page. Filters
// nothing — super admin sees every user and every unconsumed invite.
func (s *Server) adminUsersHandler(c *echo.Context) error {
	ctx := c.Request().Context()
	current := userFromContext(c)

	users, err := s.auth.Users.List(ctx)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list users", err)
	}
	sort.SliceStable(users, func(i, j int) bool { return users[i].Email < users[j].Email })

	rows := make([]adminUserRow, 0, len(users))
	for _, u := range users {
		rows = append(rows, adminUserRow{
			Email:        u.Email,
			Role:         string(u.Role),
			Disabled:     u.Disabled,
			Credentials:  len(u.Credentials),
			Created:      u.Created.UTC().Format("2006-01-02"),
			IsSelf:       current != nil && current.Email == u.Email,
			MaxApps:      u.Quotas.MaxApps,
			ModelAuthor:  u.Quotas.AllowedModels[model.TierAuthor],
			ModelEditor:  u.Quotas.AllowedModels[model.TierEditor],
			ModelUtility: u.Quotas.AllowedModels[model.TierUtility],
			ModelVision:  u.Quotas.AllowedModels[model.TierVision],
		})
	}

	invites, err := s.auth.Invites.List(ctx)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "list invites", err)
	}
	inviteRows := make([]adminInviteRow, 0, len(invites))
	now := time.Now()
	for _, inv := range invites {
		if inv.UsedBy != "" || now.After(inv.Expires) {
			continue
		}
		inviteRows = append(inviteRows, adminInviteRow{
			Token: inv.Token,
			Email: inv.Email,
			Role:  string(inv.Role),
			// Full absolute URL (scheme + host + port match the admin's
			// current request) so the operator can copy a ready-to-share
			// link instead of a bare /register?invite=<token> path.
			Expires: inv.Expires.UTC().Format("2006-01-02 15:04"),
			URL:     s.adminURL(c, "/register?invite="+inv.Token),
		})
	}
	sort.SliceStable(inviteRows, func(i, j int) bool { return inviteRows[i].Email < inviteRows[j].Email })

	return s.render(c, "admin_users", adminUsersData{
		Chrome:          Chrome{Active: "admin_users"},
		Users:           rows,
		Invites:         inviteRows,
		Flash:           c.QueryParam("flash"),
		Error:           c.QueryParam("error"),
		Roles:           []string{string(auth.RoleAdmin), string(auth.RoleSuperAdmin)},
		SuggestedModels: suggestedModels,
	})
}

// adminInviteCreateHandler accepts a form post to issue a new invite.
// Body fields: email (required), role (admin | super_admin).
func (s *Server) adminInviteCreateHandler(c *echo.Context) error {
	email := auth.NormalizeEmail(c.FormValue("email"))
	role := strings.TrimSpace(c.FormValue("role"))
	if email == "" {
		return c.Redirect(http.StatusSeeOther, "/admin/users?error=email+is+required") //nolint:wrapcheck
	}
	if role == "" {
		role = string(auth.RoleAdmin)
	}
	if role != string(auth.RoleAdmin) && role != string(auth.RoleSuperAdmin) {
		return c.Redirect(http.StatusSeeOther, "/admin/users?error=invalid+role") //nolint:wrapcheck
	}
	inv, err := s.auth.Invites.Issue(c.Request().Context(), email, auth.Role(role), auth.Quotas{}, auth.DefaultInviteTTL)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "issue invite", err)
	}
	return c.Redirect(http.StatusSeeOther, fmt.Sprintf("/admin/users?flash=invite+issued+for+%s+token+%s", inv.Email, inv.Token)) //nolint:wrapcheck
}

// adminInviteRevokeHandler deletes an invite outright.
func (s *Server) adminInviteRevokeHandler(c *echo.Context) error {
	token := c.Param("token")
	if token == "" {
		return notFound()
	}
	err := s.auth.Invites.Revoke(c.Request().Context(), token)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "revoke invite", err)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=invite+revoked") //nolint:wrapcheck
}

// adminUserDisableHandler flips the Disabled bit on a user record. Refuses
// to disable the caller themselves so a super admin can't accidentally
// lock themselves out.
func (s *Server) adminUserDisableHandler(c *echo.Context) error {
	email := emailParam(c)
	if email == "" {
		return notFound()
	}
	current := userFromContext(c)
	if current != nil && current.Email == email {
		return c.Redirect(http.StatusSeeOther, "/admin/users?error=cannot+disable+yourself") //nolint:wrapcheck
	}
	ctx := c.Request().Context()
	user, err := s.auth.Users.Load(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load user", err)
	}
	user.Disabled = true
	err = s.auth.Users.Save(ctx, user)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save user", err)
	}
	// Also drop any active sessions so the next request from that user
	// can't slip through on a still-warm cookie.
	revokeErr := s.auth.Sessions.RevokeAllForUser(ctx, email)
	if revokeErr != nil {
		// Best-effort: the disable already stuck; surface the partial
		// success in the flash so the operator knows.
		return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user+disabled+but+session+revoke+failed") //nolint:wrapcheck
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user+disabled") //nolint:wrapcheck
}

// adminUserEnableHandler clears the Disabled bit. Symmetric to disable.
func (s *Server) adminUserEnableHandler(c *echo.Context) error {
	email := emailParam(c)
	if email == "" {
		return notFound()
	}
	ctx := c.Request().Context()
	user, err := s.auth.Users.Load(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load user", err)
	}
	user.Disabled = false
	err = s.auth.Users.Save(ctx, user)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save user", err)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=user+enabled") //nolint:wrapcheck
}

// emailParam reads the :email route parameter and resolves it to a canonical
// address via normalizeEmailParam.
func emailParam(c *echo.Context) string {
	return normalizeEmailParam(c.Param("email"))
}

// normalizeEmailParam URL-unescapes a raw :email path segment before
// normalizing. Server-rendered forms put the address in the path literally
// (e.g. .../bradarchie@gmail.com/disable), but the shared quotas panel builds
// its action in JS with encodeURIComponent, so the same address can arrive
// percent-encoded (.../bradarchie%40gmail.com/quotas). Echo does not decode
// path params, so without this the lookup misses and the handler 404s.
// PathUnescape is a no-op on the already-literal forms, so both encodings
// resolve to the same record.
func normalizeEmailParam(raw string) string {
	decoded, err := url.PathUnescape(raw)
	if err == nil {
		raw = decoded
	}
	return auth.NormalizeEmail(raw)
}

// adminUserRevokeSessionsHandler drops every session for the target
// user without changing the Disabled bit. Useful when a device is lost
// and the user is about to re-enroll.
func (s *Server) adminUserRevokeSessionsHandler(c *echo.Context) error {
	email := emailParam(c)
	if email == "" {
		return notFound()
	}
	err := s.auth.Sessions.RevokeAllForUser(c.Request().Context(), email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "revoke sessions", err)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=sessions+revoked") //nolint:wrapcheck
}

// otherEnabledSuperAdmins reports whether any enabled super admin other than
// excludeEmail exists. Guards the delete paths against removing the last
// operator and leaving the platform with no one able to administer it.
func (s *Server) otherEnabledSuperAdmins(ctx context.Context, excludeEmail string) (bool, error) {
	excludeEmail = auth.NormalizeEmail(excludeEmail)
	users, err := s.auth.Users.List(ctx)
	if err != nil {
		return false, fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if u.Role == auth.RoleSuperAdmin && !u.Disabled && auth.NormalizeEmail(u.Email) != excludeEmail {
			return true, nil
		}
	}
	return false, nil
}

// adminUserDeleteHandler permanently deletes a user and disposes of the sites
// they own — either cascade-deleting them or, when a transfer_to address is
// given, reassigning them to that user. Revokes the target's sessions and any
// open invite for the address, then removes the record. Refuses to delete
// yourself (self-deletion lives on /account, which clears your own cookie) and
// refuses to delete the last enabled super admin. Requires typing the target
// email to confirm.
func (s *Server) adminUserDeleteHandler(c *echo.Context) error {
	email := emailParam(c)
	if email == "" {
		return notFound()
	}
	current := userFromContext(c)
	if current != nil && current.Email == email {
		return c.Redirect(http.StatusSeeOther, "/admin/users?error=use+the+account+page+to+delete+yourself") //nolint:wrapcheck
	}
	if auth.NormalizeEmail(c.FormValue("confirm")) != email {
		return c.Redirect(http.StatusSeeOther, "/admin/users?error=confirmation+does+not+match") //nolint:wrapcheck
	}

	ctx := c.Request().Context()
	user, err := s.auth.Users.Load(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load user", err)
	}
	if handled, resp := s.refuseLastSuperAdmin(c, user); handled {
		return resp
	}

	// Dispose of the sites first (record last, so a partial failure stays
	// retryable): reassign to transfer_to when given, else cascade-delete.
	transferTo := auth.NormalizeEmail(c.FormValue("transfer_to"))
	apps, handled, resp := s.disposeOwnedSites(c, email, transferTo)
	if handled {
		return resp
	}

	revokeErr := s.auth.Sessions.RevokeAllForUser(ctx, email)
	if revokeErr != nil {
		slog.Warn("admin.user.delete.session_revoke_failed", "email", email, "err", revokeErr)
	}
	err = s.auth.Users.Delete(ctx, email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete user", err)
	}
	s.revokePendingInvitesFor(ctx, email)
	s.registry.rebuildDomainIndexLogging(ctx)

	byEmail := ""
	if current != nil {
		byEmail = current.Email
	}
	slog.Info("admin.user.delete", "email", email, "by", byEmail, "apps", apps, "transferred_to", transferTo)

	msg := "Deleted " + email
	if transferTo != "" {
		msg = fmt.Sprintf("Deleted %s; transferred %d site(s) to %s", email, apps, transferTo)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash="+urlEscape(msg)) //nolint:wrapcheck
}

// refuseLastSuperAdmin returns handled=true (with the response to send) when
// deleting user would remove the final enabled super admin, which would leave
// the platform unadministrable. A nil target role other than super_admin is a
// no-op. The redirect writes the response and returns a nil error, so the bool
// — not resp — is what tells the caller to stop.
func (s *Server) refuseLastSuperAdmin(c *echo.Context, user *auth.User) (handled bool, resp error) {
	if user.Role != auth.RoleSuperAdmin {
		return false, nil
	}
	others, err := s.otherEnabledSuperAdmins(c.Request().Context(), user.Email)
	if err != nil {
		return true, httpErr(http.StatusInternalServerError, "check super admins", err)
	}
	if !others {
		return true, c.Redirect(http.StatusSeeOther, "/admin/users?error=cannot+delete+the+last+super+admin") //nolint:wrapcheck
	}
	return false, nil
}

// disposeOwnedSites carries out the delete-user site policy: reassign every
// site owned by email to transferTo when it's set (validating the recipient
// exists, isn't the target, and isn't disabled), otherwise cascade-delete
// them. Returns the apps affected. handled=true means a validation redirect or
// error response was produced and the caller must return resp verbatim — a
// redirect writes the response and returns nil, so the bool is the stop signal.
func (s *Server) disposeOwnedSites(c *echo.Context, email, transferTo string) (apps int, handled bool, resp error) {
	ctx := c.Request().Context()
	if transferTo == "" {
		n, err := s.deleteAppsOwnedBy(ctx, email)
		if err != nil {
			return 0, true, httpErr(http.StatusInternalServerError, "delete sites", err)
		}
		return n, false, nil
	}
	if transferTo == email {
		return 0, true, c.Redirect(http.StatusSeeOther, "/admin/users?error=cannot+transfer+to+the+user+being+deleted") //nolint:wrapcheck
	}
	recipient, err := s.auth.Users.Load(ctx, transferTo)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return 0, true, c.Redirect(http.StatusSeeOther, "/admin/users?error=no+user+with+that+transfer+email") //nolint:wrapcheck
		}
		return 0, true, httpErr(http.StatusInternalServerError, "load recipient", err)
	}
	if recipient.Disabled {
		return 0, true, c.Redirect(http.StatusSeeOther, "/admin/users?error=transfer+recipient+is+disabled") //nolint:wrapcheck
	}
	n, err := s.reassignAppsOwnedBy(ctx, email, transferTo)
	if err != nil {
		return 0, true, httpErr(http.StatusInternalServerError, "transfer sites", err)
	}
	return n, false, nil
}

// adminUserQuotasHandler accepts a form post to update a user's MaxApps
// + per-tier model overrides. Empty MaxApps means "use system default";
// each empty model field means "inherit the system default for that tier".
func (s *Server) adminUserQuotasHandler(c *echo.Context) error {
	email := emailParam(c)
	if email == "" {
		return notFound()
	}
	ctx := c.Request().Context()
	user, err := s.auth.Users.Load(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load user", err)
	}
	maxAppsStr := strings.TrimSpace(c.FormValue("max_apps"))
	maxApps := 0
	if maxAppsStr != "" {
		parsed, parseErr := strconv.Atoi(maxAppsStr)
		if parseErr != nil || parsed < 0 {
			return c.Redirect(http.StatusSeeOther, "/admin/users?error=max+apps+must+be+a+non-negative+integer") //nolint:wrapcheck
		}
		maxApps = parsed
	}
	user.Quotas.MaxApps = maxApps
	user.Quotas.AllowedModels = parseTierForm(c.FormValue)
	err = s.auth.Users.Save(ctx, user)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save user", err)
	}
	return c.Redirect(http.StatusSeeOther, "/admin/users?flash=quotas+updated") //nolint:wrapcheck
}

// parseTierForm reads the four per-tier model fields off the quotas form
// and returns a canonical TierMap. Trimmed-empty fields are dropped so the
// resulting map only carries genuine overrides — empty entries fall back
// at resolve time. Returns nil when no tier was set so the JSON sidecar
// stays clean of empty objects.
//
// Takes a value-lookup function rather than *echo.Context so it can be
// unit-tested without spinning up an Echo instance.
func parseTierForm(formValue func(string) string) model.TierMap {
	fields := map[model.Tier]string{
		model.TierAuthor:  "model_author",
		model.TierEditor:  "model_editor",
		model.TierUtility: "model_utility",
		model.TierVision:  "model_vision",
	}
	out := model.TierMap{}
	for tier, field := range fields {
		v := strings.TrimSpace(formValue(field))
		if v == "" {
			continue
		}
		out[tier] = v
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
