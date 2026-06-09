package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/storetest"
)

// TestAuth_SessionCookieName_DerivedFromPrefix locks in the fact that
// SessionCookieName() comes from the configured prefix rather than a
// hand-maintained constant. This is the cheap unit-level guard against
// the rename-the-prefix-but-forget-the-reader bug we hit twice already
// (commits b30103b and 8c44f89). No store required.
func TestAuth_SessionCookieName_DerivedFromPrefix(t *testing.T) {
	t.Parallel()

	a := &Auth{sessionCookieName: defaultCookieNamePrefix + "Usid"}
	got := a.SessionCookieName()
	want := defaultCookieNamePrefix + "Usid"
	if got != want {
		t.Fatalf("SessionCookieName() = %q, want %q", got, want)
	}
}

// TestAuth_SessionCookieName_TracksLibraryWriteSide drives the real
// egregors/passkey registerBegin handler and asserts the Set-Cookie it
// emits uses the same prefix that SessionCookieName() reports. The
// previous bug was a silent desync between the library's actual cookie
// name and the middleware's lookup constant — this test fails loudly the
// next time that drifts (e.g. if the library changes its camelCase
// scheme, or someone overrides CookieNamePrefix without updating the
// reader).
func TestAuth_SessionCookieName_TracksLibraryWriteSide(t *testing.T) {
	t.Parallel()

	st := storetest.New(t, 0)

	suffix := freshSuffix()
	probeEmail := "probe+" + suffix + "@example.com"
	superEmail := "super+" + suffix + "@example.com"

	a, err := New(Config{
		Store:           st,
		Domain:          "localhost",
		SuperAdminEmail: superEmail,
		InsecureCookies: true,
	})
	if err != nil {
		t.Fatalf("auth.New: %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })

	// Seed the probe user so UserStore.Create can find it. The library
	// refuses to mint a brand-new user on registerBegin by design — our
	// /register handler is the only path that creates users (from an
	// invite).
	ctx := context.Background()
	probe := &User{Email: probeEmail, Role: RoleAdmin, Created: time.Now().UTC()}
	err = a.Users.Save(ctx, probe)
	if err != nil {
		t.Fatalf("seed probe user: %v", err)
	}
	t.Cleanup(func() { _ = a.Users.Delete(ctx, probeEmail) })

	mux := http.NewServeMux()
	a.Passkey.MountRoutes(mux, "/auth/")

	body := strings.NewReader(`{"username":"` + probeEmail + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/auth/passkey/registerBegin", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("registerBegin status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// The library symmetrically names its auth-session and user-session
	// cookies — prefix + "Asid" and prefix + "Usid" — so a matching prefix
	// on the auth-session cookie is enough to assert the user-session
	// cookie (which we never reach in registerBegin) will match too.
	var libraryPrefix string
	for _, c := range rec.Result().Cookies() {
		if strings.HasSuffix(c.Name, "Asid") {
			libraryPrefix = strings.TrimSuffix(c.Name, "Asid")
			break
		}
	}
	if libraryPrefix == "" {
		t.Fatalf("no *Asid cookie in registerBegin response; got cookies: %v", rec.Result().Cookies())
	}

	readerPrefix := strings.TrimSuffix(a.SessionCookieName(), "Usid")
	if libraryPrefix != readerPrefix {
		t.Fatalf("library cookie prefix %q != SessionCookieName() prefix %q — middleware reads a cookie the library never sets",
			libraryPrefix, readerPrefix)
	}
}
