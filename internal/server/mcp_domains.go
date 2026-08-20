package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/sync/errgroup"

	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/snapshot"
)

// Custom domains over MCP: attach, detach, diagnose, retry. Claiming a
// hostname is an ownership decision with a hijack surface, so attach_domain
// goes through the same claimDomain guards as the settings form — a domain
// that overlaps the platform domain or already belongs to another slug is
// refused on both surfaces, and the per-slug authorization gate means a
// caller can only attach to a site they own.

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

type attachDomainInput struct {
	Slug   string `json:"slug"   jsonschema:"The site slug"`
	Domain string `json:"domain" jsonschema:"The custom hostname to serve this site on (e.g. example.com or www.example.com)"`
}

func (s *Server) registerAttachDomain(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "attach_domain",
		Description: "Attach a custom hostname to a site the caller owns, so the site is served on it. Returns the exact DNS record the owner must create at their registrar plus the domain's current DNS and certificate state — the owner still has to create that record before the domain works. Idempotent: re-attaching a domain the site already serves just re-reports its status. Refused when the hostname belongs to another site or overlaps the platform's own domain.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in attachDomainInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		host, err := s.claimDomain(in.Domain, in.Slug)
		if err != nil {
			return nil, nil, err
		}

		meta := s.build.ReadMeta(ctx, in.Slug)
		added := !slices.Contains(meta.Domains, host)
		if added {
			meta, err = s.persistDomains(ctx, in.Slug, func(m *build.SiteMeta) {
				if !slices.Contains(m.Domains, host) {
					m.Domains = append(m.Domains, host)
				}
			})
			if err != nil {
				return nil, nil, err
			}
		}

		status := s.domainStatusFor(ctx, in.Slug, host, s.resolveTarget(ctx, in.Slug))
		// Pre-warm only once DNS already points here. The web form fires
		// issuance for every newly-added domain because a person adding one has
		// usually set DNS up first; an agent typically attaches before the owner
		// has touched their registrar, and an ACME attempt that cannot possibly
		// pass the challenge only burns a Let's Encrypt rate-limit slot and
		// leaves a recorded failure that makes the next get_domain_status read
		// "failed" instead of the truthful "waiting on your DNS".
		//nolint:contextcheck // deliberately detached: the ACME round-trip must
		// outlive this tool call, not be cancelled when the client gets its reply.
		if added && status.DNS.Status == dnsOK {
			s.preWarmCerts([]string{host})
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "domain": host,
			"attached":       added,
			"domains":        meta.Domains,
			"site_host":      s.cnameTarget(in.Slug),
			"dns":            status.DNS,
			"cert":           status.Cert,
			"add_remove_url": s.manageURL(in.Slug),
			"next":           mcpDomainNext([]domainStatus{status}),
		})
	})
}

type detachDomainInput struct {
	Slug   string `json:"slug"   jsonschema:"The site slug"`
	Domain string `json:"domain" jsonschema:"The custom hostname to stop serving this site on"`
}

func (s *Server) registerDetachDomain(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "detach_domain",
		Description: "Stop serving a site the caller owns on one of its custom hostnames. The site stays reachable on its own subdomain and the DNS record at the owner's registrar is untouched — after detaching, that record points at a hostname this platform no longer answers for, so tell the owner to remove it. Idempotent: detaching a domain the site does not serve is a no-op.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in detachDomainInput) (*mcp.CallToolResult, any, error) {
		_, err := s.mcpUserAndAuthorize(ctx, in.Slug)
		if err != nil {
			return nil, nil, err
		}
		// Normalize without the claim guards: a domain already on this site must
		// stay removable even if it would no longer pass them (the platform
		// domain can change under an existing attachment).
		host, err := build.NormalizeDomain(in.Domain)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid domain: %w", err)
		}
		meta := s.build.ReadMeta(ctx, in.Slug)
		detached := slices.Contains(meta.Domains, host)
		if detached {
			meta, err = s.persistDomains(ctx, in.Slug, func(m *build.SiteMeta) {
				m.Domains = slices.DeleteFunc(slices.Clone(m.Domains), func(d string) bool { return d == host })
			})
			if err != nil {
				return nil, nil, err
			}
		}
		return mcpJSON(map[string]any{
			"ok": true, "slug": in.Slug, "domain": host,
			"detached": detached,
			"domains":  meta.Domains,
			"next": fmt.Sprintf(
				"%s no longer routes to %s — have the owner delete the DNS record pointing at %s",
				host, in.Slug, s.cnameTarget(in.Slug)),
		})
	})
}

// persistDomains snapshots, applies mutate to the sidecar, and refreshes the
// in-memory indexes — the same three steps in the same order as settingsSubmitHandler,
// because the domain index is what dispatch.go routes on: skipping the rebuild
// leaves an attached domain unroutable (and a detached one still routing)
// until the next sweep.
// The mutation runs under UpdateMeta's compare-and-set rather than a plain
// read-modify-write: the snapshot below is slow enough that a co-owner's
// concurrent settings save would otherwise be written back out of existence,
// and a domain lost that way is a site that stops routing.
func (s *Server) persistDomains(ctx context.Context, slug string, mutate func(*build.SiteMeta)) (build.SiteMeta, error) {
	s.snapshotBefore(ctx, slug, snapshot.ReasonSettings)
	meta, err := s.build.UpdateMeta(ctx, slug, mutate)
	if err != nil {
		return build.SiteMeta{}, fmt.Errorf("save domains: %w", err)
	}
	s.registry.rebuildIndexesLogging(ctx)
	return meta, nil
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
				"%q is not attached to site %q — call attach_domain first (or add it at %s)",
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
		return "no custom domains attached — call attach_domain to add one; it returns the DNS records the owner needs"
	}
	// A domain this site doesn't serve blocks everything else about it, and no
	// amount of correct DNS changes that.
	for i := range statuses {
		d := &statuses[i]
		if !d.Serves {
			if d.Detail != "" {
				return d.Detail
			}
			return d.Domain + " is not attached to this site — call attach_domain, then re-check"
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
