package server

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/buildabear/internal/auth"
)

// loginData backs templates/login.html. The page itself doesn't yet know
// who the user is — the form posts to /auth/passkey/loginBegin which
// returns the challenge.
type loginData struct {
	Active string
}

// registerData backs templates/register.html. Email + invite token are
// populated from the URL parameter; the page POSTs to /auth/passkey/* and
// then to /register/finish to consume the invite.
type registerData struct {
	Email       string
	InviteToken string
	Active      string
}

// accountCredential is one passkey row rendered on /account.
type accountCredential struct {
	ID      string
	Created string
}

// accountData backs templates/account.html.
type accountData struct {
	Email        string
	Role         string
	Credentials  []accountCredential
	Active       string
	IsSuperAdmin bool // populated by s.render via injectChrome.
}

// loginHandler renders the email-entry form. Available unauthenticated;
// the form is self-driving via JS once the user submits.
func (s *Server) loginHandler(c *echo.Context) error {
	return s.render(c, "login", loginData{Active: "login"})
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
		Email:       inv.Email,
		InviteToken: inv.Token,
		Active:      "register",
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
// another" UI. Gated by the passkey library's session cookie directly
// (commit 3 doesn't have RequireUser yet — that lands in commit 5).
func (s *Server) accountHandler(c *echo.Context) error {
	if s.auth == nil {
		return notFound()
	}
	email, ok := s.currentSessionEmail(c)
	if !ok {
		return c.Redirect(http.StatusSeeOther, "/login") //nolint:wrapcheck
	}
	user, err := s.auth.Users.LookupCached(c.Request().Context(), email)
	if err != nil {
		// Stale cookie pointing at a deleted user; clear it and bounce.
		s.auth.Passkey.Logout(c.Response(), c.Request())
		return c.Redirect(http.StatusSeeOther, "/login") //nolint:wrapcheck
	}
	creds := make([]accountCredential, 0, len(user.Credentials))
	for _, cred := range user.Credentials {
		creds = append(creds, accountCredential{
			ID:      shortenCredID(cred.ID),
			Created: time.Now().UTC().Format("2006-01-02"), // library doesn't track per-credential created time; show today's date as a placeholder until commit 6 extends User.
		})
	}
	return s.render(c, "account", accountData{
		Email:       user.Email,
		Role:        string(user.Role),
		Credentials: creds,
		Active:      "account",
	})
}

// currentSessionEmail extracts the email from the passkey library's user
// cookie. Returns ("", false) when the cookie is missing, malformed, or
// points at a session that's been deleted/expired.
//
// The library's WithSessionCookieNamePrefix does a camelCase concat of
// prefix + "Usid" (not prefix + "_usid"), so with our "bab" prefix the
// actual cookie name is "babUsid".
func (s *Server) currentSessionEmail(c *echo.Context) (string, bool) {
	if s.auth == nil {
		return "", false
	}
	ck, err := c.Request().Cookie(auth.SessionCookieName)
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
