package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	adminCookieName = "_bab_admin"
	adminCookieTTL  = 7 * 24 * time.Hour
)

// requireAdmin gates a route behind global HTTP Basic Auth. Defense in depth:
// it also rejects requests whose Host isn't the main app domain (or a
// subdomain of it), so admin endpoints can't be reached on a custom domain
// even if a route was somehow exposed there.
//
// On successful Basic Auth, a signed cookie scoped to s.domain is set. The
// cookie is what makes the edit toolbar visible on hosted subdomain pages —
// browsers don't preemptively send Basic Auth credentials across origins, so
// the cookie is the only cross-subdomain admin signal.
func (s *Server) requireAdmin(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c *echo.Context) error {
		host := stripPort(c.Request().Host)
		if !s.isMainDomainHost(host) {
			return notFound()
		}
		if s.checkAdminCookie(c.Request()) {
			return next(c)
		}
		u, p, ok := c.Request().BasicAuth()
		if !ok || !s.verifyAdminCreds(u, p) {
			c.Response().Header().Set("WWW-Authenticate", `Basic realm="buildabear"`)
			return c.NoContent(http.StatusUnauthorized)
		}
		s.setAdminCookie(c)
		return next(c)
	}
}

// isAdmin reports whether the request carries a valid admin signal. Used by
// injectEditToolbar to decide whether to render the toolbar on hosted-site
// pages.
func (s *Server) isAdmin(c *echo.Context) bool {
	r := c.Request()
	if s.checkAdminCookie(r) {
		return true
	}
	u, p, ok := r.BasicAuth()
	return ok && s.verifyAdminCreds(u, p)
}

func (s *Server) verifyAdminCreds(username, password string) bool {
	if s.adminPasswordHash == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(username), []byte(s.adminUsername)) != 1 {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(s.adminPasswordHash), []byte(password)) == nil
}

// signAdminToken returns `<expiry-unix>.<hmac>`. The HMAC key is the bcrypt
// hash of the admin password — stable across the process lifetime and rotates
// automatically when the password changes (existing cookies stop verifying).
func (s *Server) signAdminToken(expiry time.Time) string {
	payload := strconv.FormatInt(expiry.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(s.adminPasswordHash))
	mac.Write([]byte(payload))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return payload + "." + sig
}

func (s *Server) verifyAdminToken(token string) bool {
	parts := strings.SplitN(token, ".", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return false
	}
	if time.Now().After(time.Unix(expiry, 0)) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(s.adminPasswordHash))
	mac.Write([]byte(parts[0]))
	want := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(parts[1]), []byte(want))
}

func (s *Server) setAdminCookie(c *echo.Context) {
	expires := time.Now().Add(adminCookieTTL)
	http.SetCookie(c.Response(), &http.Cookie{
		Name:     adminCookieName,
		Value:    s.signAdminToken(expires),
		Path:     "/",
		Domain:   s.domain,
		Expires:  expires,
		HttpOnly: true,
		Secure:   c.Request().TLS != nil,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) checkAdminCookie(r *http.Request) bool {
	ck, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return s.verifyAdminToken(ck.Value)
}

// isMainDomainHost reports whether host is the main app domain, a loopback
// alias, or a subdomain of the main app domain. Anything else is a custom
// domain (or unknown).
func (s *Server) isMainDomainHost(host string) bool {
	if host == s.domain || fallThroughHosts[host] {
		return true
	}
	return strings.HasSuffix(host, "."+s.domain)
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i != -1 {
		return host[:i]
	}
	return host
}
