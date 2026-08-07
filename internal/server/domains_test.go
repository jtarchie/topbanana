package server

import (
	"context"
	"crypto/x509"
	"strings"
	"testing"
	"time"
)

// The CNAME target is per-site: a customer's DNS points at the site's own
// subdomain, which they can resolve and open in a browser to verify before
// they cut over. A shared platform hostname was what let a wrong target
// ("app." vs "apps.") go undetected.
func TestCNAMETarget_IsTheSitesOwnSubdomain(t *testing.T) {
	t.Parallel()

	s := &Server{domain: "apps.topbanana.dev"}
	if got, want := s.cnameTarget("fast-flame-71"), "fast-flame-71.apps.topbanana.dev"; got != want {
		t.Errorf("cnameTarget = %q; want %q", got, want)
	}

	// A deployment that fronts the app with something else still wins.
	s.systemInfo.CustomDomainCNAME = "edge.example.net"
	if got, want := s.cnameTarget("fast-flame-71"), "edge.example.net"; got != want {
		t.Errorf("cnameTarget with override = %q; want %q", got, want)
	}
}

// zoneRelativeName decides what the owner types into their provider's "Name"
// field, and whether the record can be a CNAME at all. The public-suffix cases
// are the ones a naive last-two-labels split gets wrong.
func TestZoneRelativeName(t *testing.T) {
	t.Parallel()

	cases := []struct {
		host     string
		wantName string
		wantApex bool
	}{
		{"knowhere.live", "@", true},
		{"www.knowhere.live", "www", false},
		{"shop.eu.knowhere.live", "shop.eu", false},
		{"example.co.uk", "@", true},
		{"www.example.co.uk", "www", false},
		{"WWW.Knowhere.Live", "www", false},
		{"knowhere.live.", "@", true},
	}
	for _, c := range cases {
		name, apex := zoneRelativeName(c.host)
		if name != c.wantName || apex != c.wantApex {
			t.Errorf("zoneRelativeName(%q) = (%q, %v); want (%q, %v)",
				c.host, name, apex, c.wantName, c.wantApex)
		}
	}
}

// An apex domain cannot take a CNAME. Saying so in the record itself is the
// difference between an agent dictating a record the provider will reject and
// one it will accept.
func TestExpectedRecords_ApexVersusSubdomain(t *testing.T) {
	t.Parallel()

	s := &Server{domain: "apps.topbanana.dev"}

	apex := s.expectedRecords("fast-flame-71", "knowhere.live")
	if len(apex) != 1 || apex[0].Type != "ALIAS" || apex[0].Name != "@" {
		t.Fatalf("apex records = %+v; want a single ALIAS at @", apex)
	}
	if apex[0].Value != "fast-flame-71.apps.topbanana.dev" {
		t.Errorf("apex target = %q; want the site subdomain", apex[0].Value)
	}
	if apex[0].Note == "" {
		t.Error("apex record must explain why it isn't a plain CNAME")
	}

	sub := s.expectedRecords("fast-flame-71", "www.knowhere.live")
	if len(sub) != 1 || sub[0].Type != "CNAME" || sub[0].Name != "www" {
		t.Fatalf("subdomain records = %+v; want a single CNAME at www", sub)
	}
}

// stubProber is a CertProber with canned answers, so cert reporting can be
// tested without an ACME round-trip.
type stubProber struct {
	leaf    *x509.Certificate
	attempt CertAttempt
	haveAtt bool
}

func (p stubProber) CachedCert(context.Context, string) (*x509.Certificate, bool) {
	return p.leaf, p.leaf != nil
}

func (p stubProber) EnsureCert(context.Context, string) (*x509.Certificate, error) {
	return p.leaf, nil
}

func (p stubProber) LastAttempt(context.Context, string) (CertAttempt, bool) {
	return p.attempt, p.haveAtt
}

// A cached certificate is not necessarily a working one. An expired leaf means
// renewal is failing and visitors are seeing TLS errors right now, so it must
// not report as "issued".
func TestCheckCert_ExpiredLeafIsNotIssued(t *testing.T) {
	t.Parallel()

	expired := &x509.Certificate{
		NotBefore: time.Now().Add(-90 * 24 * time.Hour),
		NotAfter:  time.Now().Add(-24 * time.Hour),
	}
	s := &Server{certs: stubProber{leaf: expired}}
	got := s.checkCert(context.Background(), "knowhere.live")
	if got.Status != certExpired {
		t.Errorf("checkCert status = %q; want %q", got.Status, certExpired)
	}
	if got.NotAfter == nil {
		t.Error("expired status should still carry NotAfter")
	}

	valid := &x509.Certificate{
		NotBefore: time.Now().Add(-24 * time.Hour),
		NotAfter:  time.Now().Add(60 * 24 * time.Hour),
	}
	s = &Server{certs: stubProber{leaf: valid}}
	if got := s.checkCert(context.Background(), "knowhere.live"); got.Status != certIssued {
		t.Errorf("checkCert status = %q; want %q", got.Status, certIssued)
	}
}

// get_domain_status takes an arbitrary hostname, so a host the caller's site
// doesn't serve must not expose the other owner's certificate state — nor
// confirm that someone else on the platform holds it. Same rule as
// authorizeSlugOwner returning "not found" for another owner's slug.
func TestDomainStatusFor_UnattachedHostLeaksNothing(t *testing.T) {
	t.Parallel()

	s := &Server{
		domain:   "apps.topbanana.dev",
		registry: &siteRegistry{},
		certs: stubProber{
			leaf:    &x509.Certificate{NotAfter: time.Now().Add(time.Hour)},
			attempt: CertAttempt{At: time.Now(), Err: "acme: someone else's private failure"},
			haveAtt: true,
		},
	}
	// The registry has the domain, but owned by a different site.
	s.registry.domainIndex = map[string]string{"knowhere.live": "another-site"}

	got := s.domainStatusFor(context.Background(), "my-site", "knowhere.live", dnsTarget{Host: "my-site.apps.topbanana.dev"})
	if got.Serves {
		t.Error("Serves must be false for a host owned by another site")
	}
	if got.Cert.Status != certUnattached {
		t.Errorf("cert status = %q; want %q with no certificate detail", got.Cert.Status, certUnattached)
	}
	if got.Cert.LastError != "" || got.Cert.NotAfter != nil {
		t.Errorf("leaked another owner's certificate state: %+v", got.Cert)
	}
	if strings.Contains(got.Detail, "another-site") || strings.Contains(got.Detail, "different site") {
		t.Errorf("detail %q reveals that another site holds the domain", got.Detail)
	}
}

// With no TLS stack wired (dev, plain HTTP), the status is explicit rather
// than an invented "pending" — the deployment genuinely has no opinion about
// certificates.
func TestCheckCert_NoTLSStack(t *testing.T) {
	t.Parallel()

	s := &Server{}
	got := s.checkCert(context.Background(), "knowhere.live")
	if got.Status != certOffTLS {
		t.Errorf("checkCert status = %q; want %q", got.Status, certOffTLS)
	}
	if got.LastError == "" {
		t.Error("tls_disabled should explain itself")
	}
}
