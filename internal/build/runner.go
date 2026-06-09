package build

import (
	"context"
	"fmt"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// Runner is the seam between the build service and the agent. The default
// implementation calls into the real LLM via internal/agent; tests inject a
// stub so the happy-path test can drive the build flow end-to-end without
// touching a live model. Run is invoked on every agent turn (initial build
// plus each lint-retry fix-up); Describe is invoked once after a successful
// build to populate the site sidecar's title + description.
//
// buildStart is captured once per build at the top of buildAndLint and
// flows unchanged into every retry, so the agent sees the same "today" all
// the way through a single build. isEdit comes from !Params.SeedSkeleton.
type Runner interface {
	Run(ctx context.Context, s *store.Store, slug, prompt string, tmpl *templates.SiteTemplate, attachments []agent.Attachment, seeds []agent.SeedToolCall, buildStart time.Time, isEdit bool, emit func(events.Event), tracker *events.Tracker) (agent.Usage, error)
	Describe(ctx context.Context, s *store.Store, slug, userPrompt string) (agent.SiteDescription, error)
}

// agentRunner is the production Runner — a thin shim over package agent that
// carries the configured ThinkingLevel into every Run call. domain / port /
// insecure are only used to compose the per-build SiteURL the agent sees
// in its BuildContext block; the real subdomain routing is owned by the
// server, not the agent.
type agentRunner struct {
	llm             adkmodel.LLM
	reasoningEffort genai.ThinkingLevel
	domain          string
	port            string
	insecure        bool
}

// NewAgentRunner constructs the production Runner around an already-resolved
// LLM client. Exposed so cmd/topbanana can wire a per-model factory without
// internal/build needing to know about internal/model.
func NewAgentRunner(llm adkmodel.LLM, reasoningEffort genai.ThinkingLevel, domain, port string, insecure bool) Runner {
	return agentRunner{
		llm:             llm,
		reasoningEffort: reasoningEffort,
		domain:          domain,
		port:            port,
		insecure:        insecure,
	}
}

// siteURL composes the canonical URL the agent should reference in og:url,
// self-links, and similar. Mirrors the scheme/port rule the auth package
// already uses for RPOrigins: insecure local dev gets http:// plus the port
// suffix (omitted for 80/443); everything else gets https:// with no port.
// An empty domain (legacy New constructor used by some tests) returns ""
// so the rendered BuildContext block stays clean.
func (r agentRunner) siteURL(slug string) string {
	if r.domain == "" {
		return ""
	}
	scheme := "https"
	port := ""
	if r.insecure {
		scheme = "http"
		if r.port != "" && r.port != "80" && r.port != "443" {
			port = ":" + r.port
		}
	}
	return fmt.Sprintf("%s://%s.%s%s", scheme, slug, r.domain, port)
}

func (r agentRunner) Run(ctx context.Context, s *store.Store, slug, prompt string, tmpl *templates.SiteTemplate, attachments []agent.Attachment, seeds []agent.SeedToolCall, buildStart time.Time, isEdit bool, emit func(events.Event), tracker *events.Tracker) (agent.Usage, error) {
	bctx := agent.BuildContext{
		Now:     buildStart,
		Slug:    slug,
		SiteURL: r.siteURL(slug),
		IsEdit:  isEdit,
	}
	usage, err := agent.Run(ctx, r.llm, agent.RunRequest{
		Store:           s,
		Slug:            slug,
		Prompt:          prompt,
		Template:        tmpl,
		Attachments:     attachments,
		Seeds:           seeds,
		ReasoningEffort: r.reasoningEffort,
		BuildContext:    bctx,
	}, emit, tracker)
	if err != nil {
		return usage, fmt.Errorf("agent run: %w", err)
	}
	return usage, nil
}

func (r agentRunner) Describe(ctx context.Context, s *store.Store, slug, userPrompt string) (agent.SiteDescription, error) {
	desc, err := agent.DescribeSite(ctx, r.llm, s, slug, userPrompt)
	if err != nil {
		return desc, fmt.Errorf("agent describe: %w", err)
	}
	return desc, nil
}
