package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// Custom-domain diagnostics. Attaching a domain is three moving parts — the
// owner's DNS, our routing index, and a Let's Encrypt cert — and until this
// file existed only the middle one was observable: meta.Domains listed a host
// whether the cert had issued, never been attempted, or permanently failed.
// The rest had to be reverse-engineered from outside with dig and
// `openssl s_client`, which is exactly the loop this replaces.
//
// Nothing here talks to a hosting provider's API: Top Banana terminates TLS
// itself via autocert (internal/server/tls.go), so cert state is a cache
// lookup and an error string we already hold.

// preWarmCerts kicks off issuance for newly-attached domains so the cert is
// ready before the first visitor, instead of the first visitor paying for the
// ACME round-trip (or seeing a TLS error when it fails). Fire-and-forget: the
// on-demand handshake path retries, and either way the attempt — success or
// the verbatim failure — lands in the tracker for get_domain_status to report.
func (s *Server) preWarmCerts(hosts []string) {
	if s.certs == nil {
		return
	}
	for _, host := range hosts {
		go func(h string) {
			_, err := s.certs.EnsureCert(context.Background(), h)
			if err != nil {
				slog.Warn("acme.prewarm_failed", "host", h, "err", err)
			}
		}(host)
	}
}

// dnsRecord is one record an owner must create at their DNS provider. Name is
// relative to their zone ("@" for the apex), which is how every provider's UI
// asks for it.
type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
	Note  string `json:"note,omitempty"`
}

// cnameTarget is the host a custom domain points at: the site's own subdomain.
// Per-site rather than one shared platform hostname so the record is
// self-evident and verifiable — the owner can dig the target directly and see
// their site — and so a typo'd target fails loudly instead of resolving to
// some other tenant's entry point. --custom-domain-cname still overrides it
// for deployments that front the app with something else.
func (s *Server) cnameTarget(slug string) string {
	if override := s.systemInfo.CustomDomainCNAME; override != "" {
		return override
	}
	return slug + "." + stripPort(s.domain)
}

// expectedRecords is the DNS an owner needs for one custom domain. Apex
// domains can't take a CNAME, so they get the ALIAS/flattening variant every
// provider spells differently — naming the constraint is what keeps an agent
// from confidently dictating an impossible record.
func (s *Server) expectedRecords(slug, host string) []dnsRecord {
	target := s.cnameTarget(slug)
	name, apex := zoneRelativeName(host)
	if apex {
		return []dnsRecord{{
			Type:  "ALIAS",
			Name:  name,
			Value: target,
			Note:  "apex domains cannot take a plain CNAME — use your provider's ALIAS / ANAME / CNAME-flattening record type",
		}}
	}
	return []dnsRecord{{Type: "CNAME", Name: name, Value: target}}
}

// zoneRelativeName splits a host into the label(s) an owner types into their
// provider's Name field, and reports whether it's the zone apex. Uses the
// public suffix list so multi-label TLDs (example.co.uk) don't get mistaken
// for a subdomain.
func zoneRelativeName(host string) (name string, apex bool) {
	host = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	zone, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || zone == host {
		return "@", true
	}
	return strings.TrimSuffix(host, "."+zone), false
}

// DNS status values. Deliberately coarse: the owner's next action differs
// between "nothing resolves" and "resolves somewhere else" (the Cloudflare
// orange-cloud case), and nothing finer changes what they'd do.
const (
	dnsOK         = "ok"               // resolves to the same addresses we do
	dnsElsewhere  = "points_elsewhere" // resolves, but not to us
	dnsUnresolved = "unresolved"       // no A/AAAA at all
	dnsUnknown    = "unknown"          // we couldn't establish our own addresses
)

// Cert status values, mapped from what we can actually observe.
const (
	certIssued  = "issued"
	certPending = "pending" // no cert yet, and no failure recorded — nobody has triggered issuance
	certFailed  = "failed"  // no cert, and the last attempt errored
	certExpired = "expired" // a cert exists but is past NotAfter — renewal is failing
	certOffTLS  = "tls_disabled"
	// certUnattached is reported for a host this site doesn't serve. Kept
	// distinct from "pending" so the answer never implies we're mid-issuance
	// for someone else's domain.
	certUnattached = "not_attached"
)

type domainDNSStatus struct {
	Status   string      `json:"status"`
	Resolved []string    `json:"resolved,omitempty"`
	CNAME    string      `json:"cname,omitempty"`
	Expected []dnsRecord `json:"expected_records"`
	Detail   string      `json:"detail,omitempty"`
}

type domainCertStatus struct {
	Status        string     `json:"status"`
	NotBefore     *time.Time `json:"not_before,omitempty"`
	NotAfter      *time.Time `json:"not_after,omitempty"`
	Issuer        string     `json:"issuer,omitempty"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

type domainStatus struct {
	Domain string           `json:"domain"`
	Serves bool             `json:"serves"`
	DNS    domainDNSStatus  `json:"dns"`
	Cert   domainCertStatus `json:"cert"`
	Detail string           `json:"detail,omitempty"`
}

// dnsLookupTimeout bounds each resolver round-trip. A status call fans out a
// handful of lookups and an agent is waiting on the result, so a slow or
// blackholed resolver has to fail fast rather than hang the tool.
const dnsLookupTimeout = 5 * time.Second

// dnsTarget is the resolved CNAME target every domain on a site is compared
// against. Resolved once per status call and reused: it's the same hostname
// for every domain, so re-resolving it per domain was pure duplicated latency.
type dnsTarget struct {
	Host  string
	Addrs []string
}

// resolveTarget looks up the site's own subdomain — whatever it resolves to is
// by definition "us", so the server never needs to discover its own public IPs
// (which it can't do reliably from behind a proxy anyway).
func (s *Server) resolveTarget(ctx context.Context, slug string) dnsTarget {
	target := dnsTarget{Host: s.cnameTarget(slug)}
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupHost(ctx, target.Host)
	if err == nil {
		slices.Sort(addrs)
		target.Addrs = addrs
	}
	return target
}

// checkDNS resolves the target and compares one host against it. The
// single-domain entry point; a multi-domain caller should resolve the target
// once with resolveTarget and use checkDNSAgainst.
func (s *Server) checkDNS(ctx context.Context, slug, host string) domainDNSStatus {
	return s.checkDNSAgainst(ctx, slug, host, s.resolveTarget(ctx, slug))
}

// checkDNSAgainst resolves host and compares its addresses against the
// already-resolved target.
func (s *Server) checkDNSAgainst(ctx context.Context, slug, host string, target dnsTarget) domainDNSStatus {
	out := domainDNSStatus{Expected: s.expectedRecords(slug, host)}

	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()

	cname, cnameErr := net.DefaultResolver.LookupCNAME(ctx, host)
	if cnameErr == nil {
		// The resolver echoes the host back when there's no CNAME; only report
		// an actual delegation.
		if trimmed := strings.TrimSuffix(cname, "."); trimmed != host {
			out.CNAME = trimmed
		}
	}

	addrs, err := net.DefaultResolver.LookupHost(ctx, host)
	if err != nil || len(addrs) == 0 {
		out.Status = dnsUnresolved
		out.Detail = host + " has no A/AAAA record — create the record below, then allow for DNS propagation"
		if err != nil {
			out.Detail = fmt.Sprintf("%s does not resolve: %v", host, err)
		}
		return out
	}
	slices.Sort(addrs)
	out.Resolved = addrs

	if len(target.Addrs) == 0 {
		out.Status = dnsUnknown
		out.Detail = fmt.Sprintf("cannot verify: the expected target %s did not resolve from this server", target.Host)
		return out
	}
	for _, a := range addrs {
		if slices.Contains(target.Addrs, a) {
			out.Status = dnsOK
			return out
		}
	}
	out.Status = dnsElsewhere
	out.Detail = fmt.Sprintf(
		"%s resolves to %s, but %s resolves to %s. If the domain is behind a CDN or reverse proxy, TLS terminates there and certificate issuance here will fail — on Cloudflare set the record to DNS only (grey cloud).",
		host, strings.Join(addrs, ", "), target.Host, strings.Join(target.Addrs, ", "))
	return out
}

// checkCert reports the certificate state for host without triggering
// issuance. Pending vs failed is the distinction that matters: pending means
// nobody has tried yet (a first visitor, or check_domain, will), failed means
// an attempt errored and retrying won't help until the cause is fixed.
func (s *Server) checkCert(ctx context.Context, host string) domainCertStatus {
	if s.certs == nil {
		return domainCertStatus{
			Status: certOffTLS,
			LastError: "this deployment does not terminate TLS (no --acme-email); " +
				"certificates are not managed here",
		}
	}
	out := domainCertStatus{}
	if attempt, ok := s.certs.LastAttempt(ctx, host); ok {
		at := attempt.At
		out.LastAttemptAt = &at
		out.LastError = attempt.Err
	}
	leaf, ok := s.certs.CachedCert(ctx, host)
	if !ok {
		out.Status = certPending
		if out.LastError != "" {
			out.Status = certFailed
		}
		return out
	}
	applyLeaf(&out, leaf)
	// A cached certificate is not the same as a working one. autocert renews
	// in the background, so a leaf that is already past NotAfter means renewal
	// has been failing and every visitor is seeing a TLS error — the one case
	// where "a cert exists" must not read as healthy.
	out.Status = certIssued
	if time.Now().After(leaf.NotAfter) {
		out.Status = certExpired
	}
	return out
}

func applyLeaf(out *domainCertStatus, leaf *x509.Certificate) {
	nb, na := leaf.NotBefore, leaf.NotAfter
	out.NotBefore, out.NotAfter = &nb, &na
	out.Issuer = leaf.Issuer.CommonName
}

// domainStatusFor assembles the whole picture for one host: whether we'd route
// it, what DNS says, and where the certificate stands.
//
// The caller may name any hostname, so a host this site doesn't serve gets DNS
// (public information — anyone can dig it) and the records it would need, but
// no certificate state and no hint about who else might hold it. Reporting
// another tenant's issuance error, or even confirming the domain is on the
// platform, would leak across owners the same way returning a real error for
// someone else's slug would.
func (s *Server) domainStatusFor(ctx context.Context, slug, host string, target dnsTarget) domainStatus {
	owner, claimed := s.registry.lookupCustomDomain(host)
	out := domainStatus{
		Domain: host,
		Serves: claimed && owner == slug,
		DNS:    s.checkDNSAgainst(ctx, slug, host, target),
	}
	if !out.Serves {
		out.Cert = domainCertStatus{Status: certUnattached}
		out.Detail = fmt.Sprintf(
			"%s is not attached to %s — call attach_domain, then re-check", host, slug)
		return out
	}
	out.Cert = s.checkCert(ctx, host)
	return out
}
