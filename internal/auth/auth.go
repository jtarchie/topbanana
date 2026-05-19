package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/egregors/passkey"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/jtarchie/buildabear/internal/store"
)

// Config wires the auth subsystem at server startup. Domain is the parent
// the cookies are scoped to and the WebAuthn RPID — every admin route is
// served from the same parent so RPID and Origin can be derived from it.
type Config struct {
	Store            *store.Store
	Domain           string
	SuperAdminEmail  string
	UserSessionTTL   time.Duration
	CookieNamePrefix string
	// InsecureCookies forces Secure=false on the library's cookies for
	// local dev (the library refuses to set them on http:// otherwise).
	// Production deployments leave this false.
	InsecureCookies bool
}

// Auth is the wired-up auth subsystem: stores, the passkey library, and the
// invite/user helpers. One per server; safe for concurrent use.
type Auth struct {
	Users    *UserStore
	Sessions *UserSessionStore
	Invites  *InviteStore
	Passkey  *passkey.Passkey
	cfg      Config
}

// New constructs the auth subsystem. Returns an error for misconfigured
// inputs (empty SuperAdminEmail, bad WebAuthn config) and for any store
// construction failure. The caller is expected to call Bootstrap once
// the server is otherwise ready.
func New(cfg Config) (*Auth, error) {
	if cfg.Store == nil {
		return nil, errors.New("auth: store required")
	}
	if cfg.Domain == "" {
		return nil, errors.New("auth: domain required")
	}
	cfg.SuperAdminEmail = NormalizeEmail(cfg.SuperAdminEmail)
	if cfg.SuperAdminEmail == "" {
		return nil, errors.New("auth: SUPER_ADMIN_EMAIL required for multi-tenancy")
	}
	if cfg.UserSessionTTL == 0 {
		cfg.UserSessionTTL = 30 * 24 * time.Hour
	}
	if cfg.CookieNamePrefix == "" {
		cfg.CookieNamePrefix = "bab"
	}

	users, err := NewUserStore(cfg.Store)
	if err != nil {
		return nil, err
	}
	sessions, err := NewUserSessionStore(cfg.Store)
	if err != nil {
		return nil, err
	}
	invites := NewInviteStore(cfg.Store)

	opts := []passkey.Option{
		passkey.WithLogger(slogLogger{}),
		passkey.WithSessionCookieNamePrefix(cfg.CookieNamePrefix),
		passkey.WithUserSessionMaxAge(cfg.UserSessionTTL),
	}
	if cfg.InsecureCookies {
		opts = append(opts, passkey.WithInsecureCookie())
	}

	pkey, err := passkey.New(passkey.Config{
		WebauthnConfig: &webauthn.Config{
			RPDisplayName: "BuildABear",
			RPID:          cfg.Domain,
			RPOrigins:     []string{"https://" + cfg.Domain},
		},
		UserStore:        users,
		AuthSessionStore: NewMemAuthSessionStore(),
		UserSessionStore: sessions,
	}, opts...)
	if err != nil {
		return nil, fmt.Errorf("auth: build passkey: %w", err)
	}

	return &Auth{
		Users:    users,
		Sessions: sessions,
		Invites:  invites,
		Passkey:  pkey,
		cfg:      cfg,
	}, nil
}

// Bootstrap seeds the super-admin record (if missing) and ensures there's
// a redeemable bootstrap invite until the super admin has bound a passkey.
// Safe to call on every startup; idempotent. Returns the invite URL the
// operator should follow on first run (empty once they're enrolled).
func (a *Auth) Bootstrap(ctx context.Context) (string, error) {
	user, err := a.Users.Load(ctx, a.cfg.SuperAdminEmail)
	if err != nil && !errors.Is(err, ErrUserNotFound) {
		return "", err
	}
	if errors.Is(err, ErrUserNotFound) {
		user = &User{
			Email:   a.cfg.SuperAdminEmail,
			Role:    RoleSuperAdmin,
			Created: time.Now().UTC(),
		}
		saveErr := a.Users.Save(ctx, user)
		if saveErr != nil {
			return "", saveErr
		}
		slog.Info("auth.bootstrap.super_admin_created", "email", a.cfg.SuperAdminEmail)
	}
	if len(user.Credentials) > 0 {
		return "", nil
	}
	invite, err := a.Invites.IssueOrReuseBootstrap(ctx, a.cfg.SuperAdminEmail)
	if err != nil {
		return "", err
	}
	url := fmt.Sprintf("https://%s/register?invite=%s", a.cfg.Domain, invite.Token)
	slog.Info("auth.bootstrap.invite_pending", "email", a.cfg.SuperAdminEmail, "url", url, "expires", invite.Expires.Format(time.RFC3339))
	return url, nil
}

// SuperAdminEmail returns the configured super-admin email. Exposed so
// middleware/handlers can compare without reaching back through cfg.
func (a *Auth) SuperAdminEmail() string { return a.cfg.SuperAdminEmail }

// slogLogger adapts slog into the passkey.Logger interface. The library
// logs at Debug/Info/Warn/Error — we map them straight through.
type slogLogger struct{}

func (slogLogger) Errorf(format string, v ...any) { slog.Error(fmt.Sprintf(format, v...)) }
func (slogLogger) Debugf(format string, v ...any) { slog.Debug(fmt.Sprintf(format, v...)) }
func (slogLogger) Infof(format string, v ...any)  { slog.Info(fmt.Sprintf(format, v...)) }
func (slogLogger) Warnf(format string, v ...any)  { slog.Warn(fmt.Sprintf(format, v...)) }
