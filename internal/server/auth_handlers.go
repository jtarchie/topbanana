package server

import (
	"context"
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
)

// loginData backs templates/login.html. The page itself doesn't yet know
// who the user is — the form posts to /auth/passkey/loginBegin which
// returns the challenge. Embeds Chrome so the shared brand + footer
// partials (which read .Year, .Active, …) render — without it the page
// 500s on the footer's `{{ if .Year }}`.
type loginData struct {
	Chrome
}

// registerData backs templates/register.html. Email + invite token are
// populated from the URL parameter; the page POSTs to /auth/passkey/* and
// then to /register/finish to consume the invite. Embeds Chrome for the
// same reason as loginData.
type registerData struct {
	Chrome
	Email       string
	InviteToken string
}

// accountCredential is one passkey row rendered on /account.
type accountCredential struct {
	// ID is the shortened, display-only fingerprint. RawID is the full
	// base64url credential id the remove form posts back so the handler can
	// match the exact credential (the shortened form is lossy / non-unique).
	ID      string
	RawID   string
	Created string
}

// accountData backs templates/account.html.
type accountData struct {
	Chrome
	Email       string
	Role        string
	Credentials []accountCredential
	// MCPEnabled reports whether the MCP endpoint is mounted on this deploy
	// (mcpSecret set). Drives which state the "Connect Claude Code" card
	// renders: the copy-paste command when true, a disabled-state note when
	// false.
	MCPEnabled bool
	// MCPCommand is the full `claude mcp add ...` line, prebuilt with this
	// server's public URL so the user can copy-paste verbatim. Empty when
	// MCP is disabled on this deploy (mcpSecret unset).
	MCPCommand string
	// IsSuperAdmin gates the operator-facing "set MCP_SECRET to enable" hint
	// shown in the disabled state — only an operator can change server config,
	// so regular users just see that MCP isn't enabled. Also hides the
	// self-delete control: super admins can't delete their own account.
	IsSuperAdmin bool
	// Flash / Error surface the outcome of a danger-zone POST (passkey removed,
	// or a refused self-delete) after the handler redirects back to /account.
	Flash string
	Error string
}

// loginHandler renders the email-entry form. Available unauthenticated;
// the form is self-driving via JS once the user submits.
func (s *Server) loginHandler(c *echo.Context) error {
	return s.render(c, "login", loginData{Chrome: Chrome{Active: "login"}})
}

// registerHandler validates an invite token and materialises (or finds)
// the user record so the subsequent WebAuthn ceremony succeeds. Renders
// the enrollment page. Returns 404 for missing/used/expired invites so
// the existence of the token isn't probeable.
func (s *Server) registerHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	token := strings.TrimSpace(c.QueryParam("invite"))
	if token == "" {
		return notFound()
	}
	ctx := c.Request().Context()
	inv, err := s.auth.Invites.Get(ctx, token)
	if err != nil {
		if errors.Is(err, auth.ErrInviteExpired) {
			return echo.NewHTTPError(http.StatusGone, "invite expired")
		}
		return notFound()
	}
	// Pre-create the user record so passkey.UserStore.Create can return it
	// when the browser hits /auth/passkey/registerBegin. Safe to call
	// repeatedly because CreateFromInvite is idempotent.
	_, err = s.auth.Users.CreateFromInvite(ctx, *inv)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "create user", err)
	}
	return s.render(c, "register", registerData{
		Chrome:      Chrome{Active: "register"},
		Email:       inv.Email,
		InviteToken: inv.Token,
	})
}

// registerFinishHandler is called by the browser after the WebAuthn
// ceremony completes. It marks the invite consumed so it can't be reused.
// The session cookie was already set by the library's loginFinish-style
// handoff inside registerFinish; this is purely the invite bookkeeping.
//
// Idempotent: re-consuming an already-consumed-by-the-same-email invite
// is a no-op.
func (s *Server) registerFinishHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	token := strings.TrimSpace(c.QueryParam("invite"))
	if token == "" {
		return notFound()
	}
	ctx := c.Request().Context()
	inv, err := s.auth.Invites.Get(ctx, token)
	if err != nil {
		// Already consumed or expired — treat as success so a duplicate
		// browser POST doesn't error the user out.
		if errors.Is(err, auth.ErrInviteNotFound) {
			return c.NoContent(http.StatusNoContent) //nolint:wrapcheck //nolint:wrapcheck
		}
		return notFound()
	}
	err = s.auth.Invites.Consume(ctx, token, inv.Email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "consume invite", err)
	}
	return c.NoContent(http.StatusNoContent) //nolint:wrapcheck
}

// logoutHandler clears the user-session cookie and the underlying S3
// record. Redirects to /login on success.
func (s *Server) logoutHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	s.auth.Passkey.Logout(c.Response(), c.Request())
	return c.Redirect(http.StatusSeeOther, "/login") //nolint:wrapcheck
}

// accountHandler renders the logged-in user's passkey list and the "add
// another" UI. Mounted under the admin group so requireUser handles the
// session lookup + disabled-user check before we get here.
func (s *Server) accountHandler(c *echo.Context) error {
	user := userFromContext(c)
	if user == nil {
		// Defensive: requireUser should have redirected, but if a future
		// route change moves /account back outside the gate we'd rather
		// 404 than panic on a nil deref below.
		return notFound()
	}
	creds := make([]accountCredential, 0, len(user.Credentials))
	for _, cred := range user.Credentials {
		creds = append(creds, accountCredential{
			ID:    shortenCredID(cred.ID),
			RawID: base64.RawURLEncoding.EncodeToString(cred.ID),
			// The library doesn't track per-credential created-time; show
			// today's date as a placeholder until User grows the field.
			Created: time.Now().UTC().Format("2006-01-02"),
		})
	}
	mcpEnabled := s.mcpSecret != ""
	mcpCmd := ""
	if mcpEnabled {
		mcpCmd = "claude mcp add --transport http topbanana " + s.adminURL(c, "/mcp")
	}
	return s.render(c, "account", accountData{
		Chrome:       Chrome{Active: "account"},
		Email:        user.Email,
		Role:         string(user.Role),
		Credentials:  creds,
		MCPEnabled:   mcpEnabled,
		MCPCommand:   mcpCmd,
		IsSuperAdmin: user.Role == auth.RoleSuperAdmin,
		Flash:        c.QueryParam("flash"),
		Error:        c.QueryParam("error"),
	})
}

// accountSignOutEverywhereHandler revokes every session for the logged-in
// user — this device included — so a lost or stolen device loses access
// immediately. Reversible (sign back in with a passkey), so the client-side
// modal confirmation is enough; no typed confirmation server-side.
func (s *Server) accountSignOutEverywhereHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	user := userFromContext(c)
	if user == nil {
		return notFound()
	}
	email := auth.NormalizeEmail(user.Email)
	ctx := c.Request().Context()
	err := s.auth.Sessions.RevokeAllForUser(ctx, email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "revoke sessions", err)
	}
	// RevokeAllForUser drops the server-side records (and, since the cache
	// hardening, the in-memory entries too); Logout additionally clears this
	// browser's cookie on the response so the redirect lands on a clean slate.
	s.auth.Passkey.Logout(c.Response(), c.Request())
	slog.Info("account.signout_all", "email", email)
	return c.Redirect(http.StatusSeeOther, "/login") //nolint:wrapcheck
}

// accountDeleteHandler permanently deletes the logged-in user's account and
// cascade-deletes every site they own, then revokes their sessions and clears
// the cookie. Super admins are refused outright — an operator self-deleting
// (especially the last one) could leave the platform with no administrator and
// no in-app recovery path; another super admin must remove them instead.
// Requires the typed-email confirmation as the irreversible-action guard.
func (s *Server) accountDeleteHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	user := userFromContext(c)
	if user == nil {
		return notFound()
	}
	email := auth.NormalizeEmail(user.Email)

	if user.Role == auth.RoleSuperAdmin {
		return c.Redirect(http.StatusSeeOther, "/account?error="+urlEscape("Super-admin accounts can't be self-deleted. Ask another operator to remove you.")) //nolint:wrapcheck
	}
	if auth.NormalizeEmail(c.FormValue("confirm")) != email {
		return echo.NewHTTPError(http.StatusBadRequest, "confirmation does not match your email")
	}

	ctx := c.Request().Context()
	// Sites first, user record last: deleting the record first would strand the
	// sites under a now-userless owner that no retry could cascade.
	apps, err := s.deleteAppsOwnedBy(ctx, email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete sites", err)
	}
	revokeErr := s.auth.Sessions.RevokeAllForUser(ctx, email)
	if revokeErr != nil {
		// The account is going away regardless; a failed revoke just means a
		// stale cookie that the deleted user record will reject on next use.
		slog.Warn("account.delete.session_revoke_failed", "email", email, "err", revokeErr)
	}
	err = s.auth.Users.Delete(ctx, email)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "delete user", err)
	}
	s.revokePendingInvitesFor(ctx, email)
	s.rebuildDomainIndexLogging(ctx)
	s.auth.Passkey.Logout(c.Response(), c.Request())

	slog.Info("account.delete", "email", email, "apps", apps)
	return c.Redirect(http.StatusSeeOther, "/login?flash="+urlEscape("Account deleted")) //nolint:wrapcheck
}

// accountRemovePasskeyHandler unbinds one passkey from the logged-in user's
// account, identified by its full base64url credential id. Refuses to remove
// the last passkey — that would lock the user out for good (passkey-only auth,
// no password fallback). Reloads the record before mutating so it doesn't alias
// the cached pointer other in-flight requests may be reading.
func (s *Server) accountRemovePasskeyHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	ctxUser := userFromContext(c)
	if ctxUser == nil {
		return notFound()
	}
	email := auth.NormalizeEmail(ctxUser.Email)
	ctx := c.Request().Context()

	user, err := s.auth.Users.Load(ctx, email)
	if err != nil {
		if errors.Is(err, auth.ErrUserNotFound) {
			return notFound()
		}
		return httpErr(http.StatusInternalServerError, "load user", err)
	}
	if len(user.Credentials) <= 1 {
		return c.Redirect(http.StatusSeeOther, "/account?error="+urlEscape("You can't remove your only passkey — add another first.")) //nolint:wrapcheck
	}

	id, decErr := base64.RawURLEncoding.DecodeString(strings.TrimSpace(c.FormValue("id")))
	if decErr != nil || len(id) == 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid credential id")
	}
	if !user.RemoveCredential(id) {
		return notFound()
	}
	err = s.auth.Users.Save(ctx, user)
	if err != nil {
		return httpErr(http.StatusInternalServerError, "save user", err)
	}
	slog.Info("account.passkey.removed", "email", email)
	return c.Redirect(http.StatusSeeOther, "/account?flash="+urlEscape("Passkey removed")) //nolint:wrapcheck
}

// revokePendingInvitesFor best-effort revokes every unconsumed invite for the
// address. Run during account/user deletion so a still-open invite can't be
// used to re-register the just-deleted address and resurrect the account.
func (s *Server) revokePendingInvitesFor(ctx context.Context, email string) {
	email = auth.NormalizeEmail(email)
	invites, err := s.auth.Invites.List(ctx)
	if err != nil {
		slog.Warn("invite.cleanup.list_failed", "email", email, "err", err)
		return
	}
	for _, inv := range invites {
		if inv.UsedBy == "" && auth.NormalizeEmail(inv.Email) == email {
			if revErr := s.auth.Invites.Revoke(ctx, inv.Token); revErr != nil {
				slog.Warn("invite.cleanup.revoke_failed", "token", inv.Token, "err", revErr)
			}
		}
	}
}

// currentSessionEmail extracts the email from the passkey library's user
// cookie. Returns ("", false) when the cookie is missing, malformed, or
// points at a session that's been deleted/expired.
//
// The cookie name is derived from the configured prefix on the auth
// instance (see auth.Auth.SessionCookieName) rather than a hand-coded
// constant — the library's camelCase concat means a renamed prefix
// silently changes the cookie name on the write side.
func (s *Server) currentSessionEmail(c *echo.Context) (string, bool) {
	if s.auth == nil {
		return "", false
	}
	ck, err := c.Request().Cookie(s.auth.SessionCookieName())
	if err != nil {
		return "", false
	}
	data, ok := s.auth.Sessions.Get(ck.Value)
	if !ok {
		return "", false
	}
	return string(data.UserID), true
}

// shortenCredID renders a credential ID as a short fingerprint suitable
// for a list UI. Returns the raw base64url for now; commit 6 can swap in
// a friendlier display name once we capture one at registration time.
func shortenCredID(id []byte) string {
	const maxLen = 24
	s := base64.RawURLEncoding.EncodeToString(id)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "…"
}
