package server_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jtarchie/topbanana/internal/build"
)

// Attaching a domain is the one MCP write that changes what the platform will
// answer for on the public internet, so the round trip has to prove three
// things: the claim persists to the sidecar (the routing index is rebuilt from
// it, so an in-memory-only attach is lost on restart), the guards that make it
// an ownership decision hold, and detach actually gives the hostname back.
func TestMCP_AttachDetachDomain_RoundTrip(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	slug := freshSlug(t)
	const owner = "owner@example.com"

	// A rival site holding a domain, seeded before the server so the startup
	// index rebuild sees the claim.
	other := freshSlug(t)
	writeMeta(t, ctx, st, other, build.SiteMeta{OwnerID: "someone@example.com", Domains: []string{"taken.example"}})

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, owner)
	seedUser(t, authBlobs, owner)
	session := connectMCP(t, srv, owner)

	out := mustCallTool(t, session, "attach_domain", map[string]any{
		"slug": slug, "domain": "  WWW.Knowhere.Example  ",
	})
	// Normalized, not stored verbatim — the routing index is keyed on the
	// lowercased host the dispatcher sees.
	if out["domain"] != "www.knowhere.example" {
		t.Errorf("domain = %v; want the normalized host", out["domain"])
	}
	if out["attached"] != true {
		t.Errorf("attached = %v; want true on a first attach", out["attached"])
	}
	// The payoff of doing this over MCP: the record to create comes back with
	// the attach, so the agent never has to describe it from memory.
	dns, _ := json.Marshal(out["dns"])
	if !strings.Contains(string(dns), "CNAME") || !strings.Contains(string(dns), slug+".localhost") {
		t.Errorf("attach did not return the required DNS record: %s", dns)
	}

	meta := readSiteMeta(t, ctx, st, slug)
	if len(meta.Domains) != 1 || meta.Domains[0] != "www.knowhere.example" {
		t.Fatalf("sidecar domains = %v; want the attached host persisted", meta.Domains)
	}

	// Idempotent: an agent re-running its own step must not duplicate the host.
	out = mustCallTool(t, session, "attach_domain", map[string]any{"slug": slug, "domain": "www.knowhere.example"})
	if out["attached"] != false {
		t.Errorf("attached = %v; want false on a re-attach", out["attached"])
	}
	if meta := readSiteMeta(t, ctx, st, slug); len(meta.Domains) != 1 {
		t.Errorf("re-attach duplicated the domain: %v", meta.Domains)
	}

	// The platform's own domain is not claimable — that would shadow a tenant
	// subdomain (or the app itself) with someone else's site.
	res := callTool(t, session, "attach_domain", map[string]any{"slug": slug, "domain": "evil.localhost"})
	if !res.IsError {
		t.Errorf("attaching a platform subdomain should be refused, got: %s", toolText(res))
	}

	// A domain held by another slug belongs to that owner; MCP must not be a
	// way around the same guard the settings form enforces.
	res = callTool(t, session, "attach_domain", map[string]any{"slug": slug, "domain": "taken.example"})
	if !res.IsError {
		t.Errorf("attaching another site's domain should be refused, got: %s", toolText(res))
	}

	out = mustCallTool(t, session, "detach_domain", map[string]any{"slug": slug, "domain": "www.knowhere.example"})
	if out["detached"] != true {
		t.Errorf("detached = %v; want true", out["detached"])
	}
	if meta := readSiteMeta(t, ctx, st, slug); len(meta.Domains) != 0 {
		t.Errorf("sidecar domains = %v; want empty after detach", meta.Domains)
	}
	out = mustCallTool(t, session, "detach_domain", map[string]any{"slug": slug, "domain": "www.knowhere.example"})
	if out["detached"] != false {
		t.Errorf("detached = %v; want false when the domain was not attached", out["detached"])
	}
}

// A caller may only attach to a site they own, and a non-owner must not be
// able to distinguish "not yours" from "does not exist".
func TestMCP_AttachDomain_NonOwnerGetsNotFound(t *testing.T) {
	st := minioStore(t)
	slug := freshSlug(t)

	srv, authBlobs, _ := buildMCPTestServer(t, st, slug, "owner@example.com")
	seedUser(t, authBlobs, "intruder@example.com")
	session := connectMCP(t, srv, "intruder@example.com")

	res := callTool(t, session, "attach_domain", map[string]any{"slug": slug, "domain": "knowhere.example"})
	if !res.IsError {
		t.Fatalf("non-owner attach should fail, got: %s", toolText(res))
	}
	if !strings.Contains(strings.ToLower(toolText(res)), "not found") {
		t.Errorf("error = %q; want an indistinguishable not-found", toolText(res))
	}
}
