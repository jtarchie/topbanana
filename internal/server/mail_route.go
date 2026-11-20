package server

import (
	"context"
	"net"
	"slices"
	"strings"
	"time"
)

// MailRoute is the SMTP RCPT-time resolver: {slug}.{domain} or an attached custom host, of a site with mail enabled. Same gates as subdomainMiddleware so a scanner-invented host is refused before DATA.
func (s *Server) MailRoute(host string) (slug string, ttl time.Duration, ok bool) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if sub, isSub := strings.CutSuffix(host, "."+s.domain); isSub {
		if strings.Contains(sub, ".") || validateSlug(sub) != nil || !s.registry.slugExists(sub) {
			return "", 0, false
		}
		slug = sub
	} else if slug, ok = s.registry.lookupCustomDomain(host); !ok {
		return "", 0, false
	}
	ttl, ok = s.registry.mailTTL(slug)
	if !ok {
		return "", 0, false
	}
	return slug, ttl, true
}

// Mail DNS status values; "elsewhere" is a warning, not an error: the host may deliberately keep its real mail provider.
const (
	mxOK        = "ok"
	mxElsewhere = "points_elsewhere"
	mxMissing   = "missing"
)

type domainMailStatus struct {
	Status   string    `json:"status"`
	MX       []string  `json:"mx,omitempty"`
	Expected dnsRecord `json:"expected_record"`
	Detail   string    `json:"detail,omitempty"`
}

// checkMX relies on the resolver following CNAMEs: a www host CNAME'd to {slug}.{domain} inherits our wildcard MX with no record of its own.
func checkMX(ctx context.Context, host, mxHost string) domainMailStatus {
	out := domainMailStatus{
		Status:   mxMissing,
		Expected: dnsRecord{Type: "MX", Name: host, Value: "10 " + mxHost, Note: "only needed when the host is not a CNAME to the site's own address"},
	}
	ctx, cancel := context.WithTimeout(ctx, dnsLookupTimeout)
	defer cancel()
	mxs, err := net.DefaultResolver.LookupMX(ctx, host)
	if err != nil || len(mxs) == 0 {
		out.Detail = "no MX record: mail to this host is not delivered anywhere"
		return out
	}
	for _, mx := range mxs {
		out.MX = append(out.MX, strings.TrimSuffix(mx.Host, "."))
	}
	if slices.Contains(out.MX, mxHost) {
		out.Status = mxOK
		return out
	}
	out.Status = mxElsewhere
	out.Detail = "MX points at another mail provider; switching it to " + mxHost + " moves all of this host's mail here"
	return out
}
