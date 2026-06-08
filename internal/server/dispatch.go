package server

import (
	"net/http"
	"strings"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/auth"
)

// methodOverrideMiddleware lets an HTML form — which can only emit GET or POST —
// drive a PATCH/PUT/DELETE route by carrying the intended verb in a `_method`
// field (urlencoded body) or the X-HTTP-Method-Override header (fetch clients).
// It runs Pre (before routing) so the router matches on the rewritten method.
// Only POST is eligible, and only the three mutating overrides are honored, so a
// stray field can't downgrade a POST to GET. The body is read only for
// urlencoded posts, leaving multipart upload/attachment routes untouched.
func methodOverrideMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			r := c.Request()
			if r.Method == http.MethodPost {
				override := r.Header.Get("X-HTTP-Method-Override")
				if override == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
					override = r.PostFormValue("_method")
				}
				switch strings.ToUpper(override) {
				case http.MethodPut, http.MethodPatch, http.MethodDelete:
					r.Method = strings.ToUpper(override)
				}
			}
			return next(c)
		}
	}
}

// subdomainMiddleware dispatches by Host:
//
//  1. main domain (or loopback) → admin routes (gated by requireAdmin).
//  2. `*.<domain>` subdomain    → proxy/api for that slug.
//  3. registered custom domain  → proxy/api for the owning slug, with the
//     custom-domain flag set so cache headers go public and the toolbar
//     stays hidden.
//  4. anything else             → 404 (don't let unknown Host headers fall
//     through to admin routes — that's the leak we're closing).
//
// Path-based dispatch inside cases 2 and 3:
//  1. /api/{name}  → apiHandler (only when the template enabled functions)
//  2. anything else → proxyHandler (static)
func (s *Server) subdomainMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			host := stripPort(c.Request().Host)

			if host == s.domain || fallThroughHosts[host] {
				return next(c)
			}

			if slug, ok := strings.CutSuffix(host, "."+s.domain); ok {
				// Same three-gate check as HostAllowed: reject nested
				// subdomains, invalid slug shape, and slugs that don't
				// correspond to a real app. Stops scanner traffic from
				// generating per-request log noise and from waking up the S3
				// lookup path.
				if strings.Contains(slug, ".") || validateSlug(slug) != nil || !s.registry.slugExists(slug) {
					return notFound()
				}
				return s.dispatchSite(c, slug)
			}

			if slug, ok := s.registry.lookupCustomDomain(host); ok {
				c.Set("custom_domain", true)
				return s.dispatchSite(c, slug)
			}

			return notFound()
		}
	}
}

// pathRouteHandler serves a hosted site via /s/:slug[/...], reusing the
// dispatchSite path that the subdomain route also funnels through. Slug
// validation mirrors subdomainMiddleware so an unknown or malformed slug
// returns 404 instead of leaking the existence of admin routes. The URL
// path is rewritten in place so proxyHandler / apiHandler see the relative
// site path (e.g. /about.html, /api/submit), exactly as they would on the
// subdomain.
func (s *Server) pathRouteHandler(c *echo.Context) error {
	slug := c.Param("slug")
	if validateSlug(slug) != nil || !s.registry.slugExists(slug) {
		return notFound()
	}

	req := c.Request()
	rest := strings.TrimPrefix(req.URL.Path, "/s/"+slug)
	if rest == "" {
		rest = "/"
	}
	req.URL.Path = rest

	return s.dispatchSite(c, slug)
}

// dispatchSite routes a request that's already been mapped to a slug to either
// /api or the static proxy. Private slugs are gated here — anyone but the
// owner (or a super admin) gets a 404 so the existence of a private site
// can't be inferred from the status code.
func (s *Server) dispatchSite(c *echo.Context, slug string) error {
	if s.registry.isPrivate(slug) && !s.callerCanViewPrivate(c, slug) {
		return notFound()
	}
	reqPath := c.Request().URL.Path
	if name, ok := strings.CutPrefix(reqPath, "/api/"); ok {
		return s.apiHandler(c, slug, name)
	}
	return s.proxyHandler(c, slug)
}

// callerCanViewPrivate answers whether the request's session belongs to a
// user permitted to see a private site. The subdomain path doesn't go
// through requireUser so we read the session cookie directly — the same
// cookie the admin chain uses, just resolved inline without erroring on
// miss. Super admins always pass; otherwise the email must match the
// recorded owner.
func (s *Server) callerCanViewPrivate(c *echo.Context, slug string) bool {
	email, ok := s.currentSessionEmail(c)
	if !ok {
		return false
	}
	if email == s.registry.ownerOf(slug) {
		return true
	}
	if s.auth == nil {
		return false
	}
	u, err := s.auth.Users.LookupCached(c.Request().Context(), email)
	if err != nil {
		return false
	}
	return u.Role == auth.RoleSuperAdmin
}

// hstsMiddleware advertises HSTS only when the request actually arrived over
// TLS (c.Request().TLS != nil — true on the autocert HTTPS listener, false
// on `task local` plain HTTP). Two years is the long-standing recommendation;
// includeSubDomains covers per-slug subdomains. Preload is intentionally off
// — wait for a few weeks of clean issuance before opting in (it's irrevocable
// for ~12 months).
func hstsMiddleware() echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			if c.Request().TLS != nil {
				c.Response().Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
			}
			return next(c)
		}
	}
}

// HostAllowed mirrors subdomainMiddleware's dispatch table: the host is
// recognised if it's the main domain (or a loopback fall-through), a *valid*
// slug subdomain (single label that passes validateSlug), or a registered
// custom domain. Exported so the autocert HostPolicy can reuse the same
// source of truth — without the validateSlug check, scanners pummeling us
// with hosts like "whm.whm.x.apps.jtarchie.com" would each trigger a new LE
// issuance attempt and burn the 50/week per-registered-domain rate limit.
func (s *Server) HostAllowed(host string) bool {
	host = strings.ToLower(stripPort(host))
	if host == s.domain || fallThroughHosts[host] {
		return true
	}
	if prefix, ok := strings.CutSuffix(host, "."+s.domain); ok {
		// Three gates in order of cost: cheap shape check first (rejects
		// nested subdomains), then validateSlug (cheap), then the slug
		// existence check (in-memory map lookup) — anything past the shape
		// gate that doesn't name a real app gets refused before autocert
		// ever calls Let's Encrypt. Without this last gate, a scanner
		// hitting "foo.apps.jtarchie.com" triggers a real LE issuance and
		// burns a slot in the 50/week per-registered-domain rate limit.
		if strings.Contains(prefix, ".") {
			return false
		}
		if validateSlug(prefix) != nil {
			return false
		}
		return s.registry.slugExists(prefix)
	}
	_, ok := s.registry.lookupCustomDomain(host)
	return ok
}
