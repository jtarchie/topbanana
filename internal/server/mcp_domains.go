package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/jtarchie/topbanana/internal/build"
)

// The read + retry half of custom domains over MCP. Attaching and detaching a
// domain stays in the web UI: claiming a hostname is an ownership decision
// with a hijack surface (parseDomains enforces the cross-site guards), and the
// expensive part of the job was never the click — it was having no way to see
// whether DNS and the certificate had actually landed.

func (s *Server) registerGetDomainStatus(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_domain_status",
		Description: "Diagnose the custom domains on a site the caller owns: the exact DNS records the domain needs, whether it currently resolves to this platform, and the TLS certificate state (issued / pending / failed) with the verbatim issuance error. Read-only — it never triggers issuance. Call this before telling a user to change anything; use check_domain to retry after they fix their DNS.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in getDomainStatusInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		hosts, err := s.domainsToInspect(ctx, in.Slug, in.Domain)
		if err != nil {
			return nil, nil, err
		}
		out := s.domainStatuses(ctx, in.Slug, hosts)
		return mcpJSON(map[string]any{
			"slug":           in.Slug,
			"site_host":      s.cnameTarget(in.Slug),
			"domains":        out,
			"add_remove_url": s.manageURL(in.Slug),
			"next":           mcpDomainNext(out),
		})
	})
}

type getDomainStatusInput struct {
	Slug   string `json:"slug"             jsonschema:"The site slug"`
	Domain string `json:"domain,omitempty" jsonschema:"Inspect only this hostname. Omit to inspect every domain attached to the site."`
}

type checkDomainInput struct {
	Slug   string `json:"slug"   jsonschema:"The site slug"`
	Domain string `json:"domain" jsonschema:"The custom hostname to attempt certificate issuance for"`
}

func (s *Server) registerCheckDomain(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "check_domain",
		Description: "Force a TLS certificate issuance attempt for one custom domain on a site the caller owns, and return the result — the verbatim ACME error when it fails. Use after the owner has corrected their DNS; issuance also happens automatically on the first visit, so this is a way to find out now rather than a required step. Slow (a live ACME round-trip) and rate-limited upstream by Let's Encrypt: don't call it in a loop.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in checkDomainInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		host, err := build.NormalizeDomain(in.Domain)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid domain: %w", err)
		}
		if s.certs == nil {
			return nil, nil, errNoTLSHere
		}
		// Refuse a domain we wouldn't route: autocert's HostPolicy would reject
		// it anyway, and a targeted error beats a generic ACME failure.
		if owner, ok := s.registry.lookupCustomDomain(host); !ok || owner != in.Slug {
			return nil, nil, fmt.Errorf(
				"%q is not attached to site %q — add it on the manage page first (%s)",
				host, in.Slug, s.manageURL(in.Slug))
		}

		// Report DNS alongside the attempt: an issuance failure is nearly
		// always a DNS problem, and having both in one result saves the
		// follow-up call.
		dns := s.checkDNS(ctx, in.Slug, host)
		_, issueErr := s.certs.EnsureCert(ctx, host)
		status := s.checkCert(ctx, host)
		res := map[string]any{
			"slug": in.Slug, "domain": host,
			"ok":   issueErr == nil,
			"dns":  dns,
			"cert": status,
		}
		if issueErr != nil {
			res["error"] = issueErr.Error()
			res["next"] = mcpDomainRemedy(dns)
		}
		return mcpJSON(res)
	})
}

var errNoTLSHere = errors.New("this deployment does not manage TLS certificates (no ACME email configured)")

// maxDomainProbes bounds how many domains are probed at once. Each one costs a
// DNS round-trip or two; a site with a dozen domains probed serially could sit
// on the tool's 5s-per-domain budget long enough for the client to give up
// before any of it came back.
const maxDomainProbes = 8

// domainStatuses probes every host concurrently and returns them in the
// caller's order. The CNAME target is resolved once up front — it's the same
// hostname for every domain on the site.
func (s *Server) domainStatuses(ctx context.Context, slug string, hosts []string) []domainStatus {
	out := make([]domainStatus, len(hosts))
	if len(hosts) == 0 {
		return out
	}
	target := s.resolveTarget(ctx, slug)

	group := new(errgroup.Group)
	group.SetLimit(maxDomainProbes)
	for i, host := range hosts {
		group.Go(func() error {
			out[i] = s.domainStatusFor(ctx, slug, host, target)
			return nil
		})
	}
	// The probes never return an error — each failure is a reported status, so
	// one unresolvable domain must not suppress the rest.
	_ = group.Wait()
	return out
}

// domainsToInspect resolves the tool's optional domain filter against the
// site's attached domains. An explicit domain is inspected even when it isn't
// attached — "you haven't added this yet" is a diagnosis, not an error.
func (s *Server) domainsToInspect(ctx context.Context, slug, filter string) ([]string, error) {
	if filter != "" {
		host, err := build.NormalizeDomain(filter)
		if err != nil {
			return nil, fmt.Errorf("invalid domain: %w", err)
		}
		return []string{host}, nil
	}
	meta := s.build.ReadMeta(ctx, slug)
	return meta.Domains, nil
}

// manageURL is where an owner adds or removes a custom domain — returned with
// every domain result so an agent hands over a link instead of describing
// where the button is.
func (s *Server) manageURL(slug string) string {
	return s.mcpBaseURL() + "/manage/" + slug
}

// mcpDomainNext turns the statuses into the single most useful next action.
// Ordered by what blocks what: DNS first (a certificate cannot issue until the
// challenge reaches us), then the certificate, then nothing to do.
func mcpDomainNext(statuses []domainStatus) string {
	if len(statuses) == 0 {
		return "no custom domains attached — add one on the manage page, then re-run get_domain_status for the DNS records"
	}
	// A domain this site doesn't serve blocks everything else about it, and no
	// amount of correct DNS changes that.
	for i := range statuses {
		d := &statuses[i]
		if !d.Serves {
			if d.Detail != "" {
				return d.Detail
			}
			return d.Domain + " is not attached to this site — add it on the manage page, then re-check"
		}
	}
	for i := range statuses {
		d := &statuses[i]
		if d.DNS.Status != dnsOK {
			return fmt.Sprintf("%s: fix DNS first — %s", d.Domain, mcpDomainRemedy(d.DNS))
		}
	}
	for i := range statuses {
		d := &statuses[i]
		switch d.Cert.Status {
		case certFailed:
			return fmt.Sprintf("%s: DNS looks right but issuance failed (%s) — fix the cause, then call check_domain",
				d.Domain, d.Cert.LastError)
		case certPending:
			return d.Domain + ": DNS looks right and no certificate has been issued yet — call check_domain to issue one now"
		case certExpired:
			return fmt.Sprintf(
				"%s: the certificate expired at %s and renewal is not succeeding — visitors are seeing a TLS error right now; call check_domain for the reason",
				d.Domain, mcpFormatExpiry(d.Cert.NotAfter))
		case certOffTLS:
			return d.Domain + ": DNS resolves here, but this deployment does not manage TLS certificates — nothing further to do here"
		}
	}
	return "all domains resolve here and have a certificate — nothing to do"
}

// mcpFormatExpiry renders a cert expiry for the next-action string, tolerating
// the nil that a status without a parsed leaf carries.
func mcpFormatExpiry(at *time.Time) string {
	if at == nil {
		return "an unknown time"
	}
	return at.UTC().Format(time.RFC3339)
}

// mcpDomainRemedy states the corrective action for a DNS status, naming the
// exact record rather than describing it.
func mcpDomainRemedy(dns domainDNSStatus) string {
	if dns.Status == dnsOK {
		return "DNS is correct"
	}
	var remedy strings.Builder
	remedy.WriteString(dns.Detail)
	for _, r := range dns.Expected {
		fmt.Fprintf(&remedy, " | required record: %s %s -> %s", r.Type, r.Name, r.Value)
		if r.Note != "" {
			remedy.WriteString(" (" + r.Note + ")")
		}
	}
	return remedy.String()
}
