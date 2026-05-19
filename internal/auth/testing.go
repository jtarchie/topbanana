package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/egregors/passkey"
)

// SessionCookieName is the user-session cookie the egregors/passkey
// library writes after a successful login. We need this name on the
// read side too (middleware, /account, tests). The library's
// WithSessionCookieNamePrefix does a camelCase concat of
// prefix + "Usid" (not prefix + "_usid"), so with our "bab" prefix the
// real cookie name is "babUsid".
const SessionCookieName = "babUsid"

// InjectTestSession is a test-only entry point that creates a session
// for the given email, returning the cookie value. Used by the server
// e2e tests so they don't have to simulate the full WebAuthn ceremony to
// reach an authenticated state. Builds the user record if missing.
//
// Not for production use — guards live on the call sites by virtue of
// every caller being in *_test.go.
func (a *Auth) InjectTestSession(ctx context.Context, email string, role Role) (string, error) {
	email = NormalizeEmail(email)
	_, loadErr := a.Users.Load(ctx, email)
	if loadErr != nil {
		// errors.Is would pull in errors here for one line; fall through
		// on any miss because Save is idempotent and that's the only
		// realistic error we'd see during a test setup.
		saveErr := a.Users.Save(ctx, &User{
			Email:   email,
			Role:    role,
			Created: time.Now().UTC(),
		})
		if saveErr != nil {
			return "", saveErr
		}
	}
	token, err := a.Sessions.Create(passkey.UserSessionData{ //nolint:contextcheck // passkey.SessionStore.Create has no ctx parameter
		UserID:  []byte(email),
		Expires: time.Now().Add(24 * time.Hour),
	})
	if err != nil {
		return "", fmt.Errorf("auth: inject session: %w", err)
	}
	return token, nil
}
