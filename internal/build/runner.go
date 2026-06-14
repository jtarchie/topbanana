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
	Run(ctx context.Context, s *store.Store, req RunRequest, emit func(events.Event), tracker *events.Tracker) (agent.Usage, error)
	Describe(ctx context.Context, s *store.Store, slug, userPrompt string) (agent.SiteDescription, error)
}

// RunRequest bundles the per-turn inputs to Runner.Run. ctx, the store, emit,
// and tracker stay positional as the ambient collaborators; everything that
// describes the turn lives here so call sites and the half-dozen test stubs
// read clearly instead of threading an 11-value positional list. Prompt and
// Seeds change between the initial build and each lint-retry fix-up; BuildStart
// is captured once and flows unchanged so the agent sees the same "today".
type RunRequest struct {
	Slug        string
	Prompt      string
	Template    *templates.SiteTemplate
	Attachments []agent.Attachment
	Seeds       []agent.SeedToolCall
	BuildStart  time.Time
	IsEdit      bool
	// OnInstruction, when non-nil, is invoked once with the rendered system
	// instruction the agent will see — after BuildContext is constructed and
	// before the LLM is called. Used by buildAndLint to stamp the recorder via
	// rec.SetSystemPrompt so the debug page can surface the same string the
	// model saw. Stubs (test runners) leave it nil; production agentRunner
	// invokes it.
	OnInstruction func(string)
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

func (r agentRunner) Run(ctx context.Context, s *store.Store, req RunRequest, emit func(events.Event), tracker *events.Tracker) (agent.Usage, error) {
	bctx := agent.BuildContext{
		Now:     req.BuildStart,
		Slug:    req.Slug,
		SiteURL: r.siteURL(req.Slug),
		IsEdit:  req.IsEdit,
	}
	agentReq := agent.RunRequest{
		Store:           s,
		Slug:            req.Slug,
		Prompt:          req.Prompt,
		Template:        req.Template,
		Attachments:     req.Attachments,
		Seeds:           req.Seeds,
		ReasoningEffort: r.reasoningEffort,
		BuildContext:    bctx,
	}

	if req.OnInstruction != nil {
		req.OnInstruction(agent.InstructionFor(agentReq))
	}

	usage, err := agent.Run(ctx, r.llm, agentReq, emit, tracker)
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
