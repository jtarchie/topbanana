package server

import (
	"strings"
	"testing"
	"time"
)

// mcpDomainNext is what an agent acts on, so the ordering is the contract: a
// certificate cannot issue while DNS is wrong, and telling someone to retry
// issuance before they fix their records is the loop this whole feature
// exists to prevent.
func TestMCPDomainNext_DNSBlocksCert(t *testing.T) {
	t.Parallel()

	badDNS := domainStatus{
		Domain: "knowhere.live",
		Serves: true,
		DNS: domainDNSStatus{
			Status:   dnsElsewhere,
			Detail:   "resolves to a CDN",
			Expected: []dnsRecord{{Type: "ALIAS", Name: "@", Value: "site.apps.topbanana.dev"}},
		},
		// A certificate could plausibly be reported as merely pending here; DNS
		// still has to come first.
		Cert: domainCertStatus{Status: certPending},
	}
	got := mcpDomainNext([]domainStatus{badDNS})
	if !strings.Contains(got, "fix DNS first") {
		t.Errorf("next = %q; want it to lead with DNS", got)
	}
	// The remedy must carry the literal record, not a description of it.
	if !strings.Contains(got, "ALIAS @ -> site.apps.topbanana.dev") {
		t.Errorf("next = %q; want the exact record spelled out", got)
	}

	failed := domainStatus{
		Domain: "knowhere.live",
		Serves: true,
		DNS:    domainDNSStatus{Status: dnsOK},
		Cert:   domainCertStatus{Status: certFailed, LastError: "acme: authorization failed"},
	}
	got = mcpDomainNext([]domainStatus{failed})
	if !strings.Contains(got, "acme: authorization failed") {
		t.Errorf("next = %q; want the verbatim issuance error", got)
	}
	if !strings.Contains(got, "check_domain") {
		t.Errorf("next = %q; want it to name the retry tool", got)
	}

	healthy := domainStatus{
		Domain: "knowhere.live",
		Serves: true,
		DNS:    domainDNSStatus{Status: dnsOK},
		Cert:   domainCertStatus{Status: certIssued},
	}
	if got := mcpDomainNext([]domainStatus{healthy}); !strings.Contains(got, "nothing to do") {
		t.Errorf("next = %q; want a clean all-good result", got)
	}

	// An expired certificate is the case that used to read as healthy: the
	// leaf is still cached, so status was "issued" and the next action was
	// "nothing to do" while every visitor got a TLS error.
	expiredAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	expired := domainStatus{
		Domain: "knowhere.live",
		Serves: true,
		DNS:    domainDNSStatus{Status: dnsOK},
		Cert:   domainCertStatus{Status: certExpired, NotAfter: &expiredAt},
	}
	got = mcpDomainNext([]domainStatus{expired})
	if strings.Contains(got, "nothing to do") {
		t.Errorf("next = %q; an expired certificate is not nothing to do", got)
	}
	if !strings.Contains(got, "2026-01-02T03:04:05Z") {
		t.Errorf("next = %q; want the expiry timestamp", got)
	}

	// A deployment that doesn't manage TLS must not claim the domain has a
	// certificate just because DNS resolves.
	offTLS := domainStatus{
		Domain: "knowhere.live",
		Serves: true,
		DNS:    domainDNSStatus{Status: dnsOK},
		Cert:   domainCertStatus{Status: certOffTLS},
	}
	if got := mcpDomainNext([]domainStatus{offTLS}); strings.Contains(got, "have a certificate") {
		t.Errorf("next = %q; must not claim a certificate exists", got)
	}

	// A host this site doesn't serve outranks everything else, and never
	// returns an empty string even if Detail was not populated.
	unattached := domainStatus{Domain: "someone-else.com", Serves: false}
	got = mcpDomainNext([]domainStatus{unattached})
	if got == "" || !strings.Contains(got, "not attached") {
		t.Errorf("next = %q; want an attach-it-first instruction", got)
	}

	// No domains at all is a diagnosis too — the site simply hasn't had one
	// attached, and the agent can do that itself rather than handing off.
	if got := mcpDomainNext(nil); !strings.Contains(got, "attach_domain") {
		t.Errorf("next = %q; want it to name the attach tool", got)
	}
}
