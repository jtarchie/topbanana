package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/egregors/passkey"
	"github.com/go-webauthn/webauthn/webauthn"

	"github.com/jtarchie/topbanana/internal/store"
)

// defaultCookieNamePrefix is the prefix passed to the egregors/passkey
// library by default. The library's WithSessionCookieNamePrefix runs
// this through camelCaseConcat with "usid" to produce the actual cookie
// name — so a fresh login lands a cookie named defaultCookieNamePrefix
// + "Usid" (e.g. "bhUsid"). Anything that reads the cookie must derive
// its name from this constant (or, preferably, call Auth.SessionCookieName)
// so renames stay in lockstep with the write side.
const defaultCookieNamePrefix = "bh"

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
	// Production deployments leave this false. When true, RPOrigins is
	// also extended with http://{Domain} (and the port-bearing variant
	// if Port is set) so WebAuthn accepts the browser's plain-HTTP
	// origin during registration/login.
	InsecureCookies bool
	// Port is the HTTP listen port; only used to build the local-dev
	// RPOrigin when InsecureCookies is true. Empty means "no port suffix"
	// (the default 80/443 case where the browser omits the port from
	// Origin headers).
	Port string
	// QuotaDefaults are the platform-wide fallbacks applied when a user
	// record's Quotas struct is zero-valued. Wired from CLI flags.
	QuotaDefaults QuotaDefaults
}

// Auth is the wired-up auth subsystem: stores, the passkey library, and the
// invite/user helpers. One per server; safe for concurrent use.
type Auth struct {
	Users    *UserStore
	Sessions *UserSessionStore
	Invites  *InviteStore
	Passkey  *passkey.Passkey
	cfg      Config
	// sessionCookieName is the name the egregors/passkey library actually
	// writes after loginFinish, computed once at construction time from the
	// configured prefix. Exposed via SessionCookieName().
	sessionCookieName string
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
		cfg.CookieNamePrefix = defaultCookieNamePrefix
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
			RPDisplayName: "Top Banana",
			RPID:          cfg.Domain,
			RPOrigins:     buildRPOrigins(cfg.Domain, cfg.Port, cfg.InsecureCookies),
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
		// camelCaseConcat(prefix, "usid") for a lowercase prefix is just
		// prefix + "Usid". Keep this in sync with the library's option, not
		// with a hand-maintained constant — commits b30103b and 8c44f89
		// were both the "constant drifted from prefix" bug.
		sessionCookieName: cfg.CookieNamePrefix + "Usid",
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

// buildRPOrigins assembles the WebAuthn RPOrigins list. Production always
// gets the https://{domain} form. When insecure is set (local HTTP dev),
// also add the http://{domain} and http://{domain}:{port} variants so
// the browser's Origin header matches; the webauthn library does exact
// string matching. The port-bearing form is omitted for the default
// HTTP/HTTPS ports because browsers strip them from the Origin header.
func buildRPOrigins(domain, port string, insecure bool) []string {
	origins := []string{"https://" + domain}
	if !insecure {
		return origins
	}
	origins = append(origins, "http://"+domain)
	if port != "" && port != "80" && port != "443" {
		origins = append(origins, "http://"+domain+":"+port)
	}
	return origins
}

// SuperAdminEmail returns the configured super-admin email. Exposed so
// middleware/handlers can compare without reaching back through cfg.
func (a *Auth) SuperAdminEmail() string { return a.cfg.SuperAdminEmail }

// SessionCookieName returns the name of the user-session cookie the
// egregors/passkey library writes after a successful login. Derived from
// the CookieNamePrefix passed to passkey.WithSessionCookieNamePrefix so a
// future rename of the prefix can't desync the read side from the write
// side. The previous package-level constant did exactly that — see
// commits b30103b ("read the session cookie under the name the library
// actually sets") and 8c44f89 (project rename, which re-broke it).
func (a *Auth) SessionCookieName() string { return a.sessionCookieName }

// QuotaDefaults returns the platform fallback quotas wired at startup.
// Used by the build handler to fill in zero-valued per-user limits.
func (a *Auth) QuotaDefaults() QuotaDefaults { return a.cfg.QuotaDefaults }

// slogLogger adapts slog into the passkey.Logger interface. The library
// logs at Debug/Info/Warn/Error — we map them straight through.
type slogLogger struct{}

func (slogLogger) Errorf(format string, v ...any) { slog.Error(fmt.Sprintf(format, v...)) }
func (slogLogger) Debugf(format string, v ...any) { slog.Debug(fmt.Sprintf(format, v...)) }
func (slogLogger) Infof(format string, v ...any)  { slog.Info(fmt.Sprintf(format, v...)) }
func (slogLogger) Warnf(format string, v ...any)  { slog.Warn(fmt.Sprintf(format, v...)) }
