// Package build orchestrates the per-slug build lifecycle: seed the
// template skeleton, run the agent, lint and retry on failures, and persist
// the per-site metadata sidecar.
package build

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

const (
	maxLintRetries = 3

	// DefaultBuildTimeout caps total wall-clock for the initial agent.Run
	// plus any lint-retry passes plus the lint passes themselves. The ADK
	// runner has no built-in deadline; without this a stuck LLM call or a
	// stalled network ties up a goroutine and token budget indefinitely.
	//
	// 15 minutes by default because the design-substrate prompt + DaisyUI
	// example pages produce richer HTML files (5–15 KB each), and a local
	// 26B model generating 5 KB of HTML at ~30 tok/s already takes 2-3
	// minutes per turn. Cloud-only deployments can lower this via the CLI.
	DefaultBuildTimeout = 15 * time.Minute

	// MetaFile holds the per-site sidecar (template id, creation time, custom
	// domains). Stored alongside the HTML files in the same S3 prefix so it
	// travels with the site.
	MetaFile = ".topbanana.json"
)

// legacyMetaFiles are pre-rebrand sidecar names, newest first. ReadMeta falls
// through them in order when MetaFile is absent so sites created before a
// rebrand keep their template id, custom domains, and function flags:
// `.bloomhollow.json` predates the Top Banana rebrand, `.buildabear.json`
// predates the Bloomhollow rebrand. The next successful WriteMeta migrates the
// site to MetaFile.
var legacyMetaFiles = []string{".bloomhollow.json", ".buildabear.json"}

// SiteMeta is the per-site sidecar persisted at MetaFile.
//
// EnablesFunctions is a per-site override layered on top of the template's
// default — set when a user opts in to dynamic features from the settings page
// for a site whose template didn't ship with them. Always read through
// EffectiveTemplate so the override is honoured everywhere the template's
// bit is consulted.
//
// Domains are external hostnames (e.g. `example.com`, `www.example.com`)
// that resolve to this site. Lowercased + port-stripped on write; the server
// builds a reverse Host → slug index from them.
type SiteMeta struct {
	Template         string    `json:"template"`
	Created          time.Time `json:"created"`
	Domains          []string  `json:"domains,omitempty"`
	EnablesFunctions bool      `json:"enables_functions,omitempty"`
	// EnablesPublicAPI opts the site's /api/* routes out of the same-origin
	// check applied to state-changing requests. Off by default so a freshly
	// built site can't be drive-by-spammed from another origin. Turn on for
	// genuine public APIs (webhooks, public JSON endpoints).
	EnablesPublicAPI bool   `json:"enables_public_api,omitempty"`
	Title            string `json:"title,omitempty"`
	Description      string `json:"description,omitempty"`
	// OwnerID is the canonical email of the user that owns this app. Empty on
	// pre-multi-tenancy sites; the bootstrap migration assigns those to the
	// super admin on first start.
	OwnerID string `json:"owner_id,omitempty"`
	// Private hides the site from the public web. When true the subdomain
	// (and any custom domains) return 404 to everyone except the owner and
	// super admins.
	Private bool `json:"private,omitempty"`
}

// EffectiveTemplate returns the template a build/edit/route lookup should use,
// OR-ing the per-site EnablesFunctions override on top of the template's
// default. Returns a shallow copy when the override flips the bit on so
// callers can never mutate the package-level template registry.
func EffectiveTemplate(meta SiteMeta) *templates.SiteTemplate {
	base := templates.Get(meta.Template)
	if base == nil || base.EnablesFunctions || !meta.EnablesFunctions {
		return base
	}
	out := *base
	out.EnablesFunctions = true
	return &out
}

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
	usage, err := agent.Run(ctx, r.llm, s, slug, prompt, tmpl, attachments, seeds, r.reasoningEffort, bctx, emit, tracker)
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

// Service runs builds against a Store, reporting progress through an events
// Tracker and delegating agent work to a Runner. Snapshots (when configured)
// capture the site state right before the agent runs so each build/edit is
// reversible from the History UI. editsKeep caps how many transcripts to
// retain per slug (0 disables trimming, but transcripts are still written).
// buildTimeout caps wall-clock per build; zero falls back to DefaultBuildTimeout.
// reasoningEffort is recorded in each transcript so the debug viewer can show
// which reasoning level the operator was running at the time.
//
// tierMap is the operator-configured per-tier model assignment. Per-build
// overrides on Params.Tiers layer on top via TierMap.Merge.
//
// runnersByTier is a test seam: when non-nil, runnerForTier returns the
// pinned Runner for that tier instead of consulting the cache. runner is
// the legacy single-Runner shorthand — when set it serves every tier.
type Service struct {
	store           *store.Store
	runner          Runner
	runnersByTier   map[model.Tier]Runner
	tierMap         model.TierMap
	events          *events.Tracker
	snapshot        *snapshot.Service
	editsKeep       int
	recordEdit      bool
	buildTimeout    time.Duration
	reasoningEffort genai.ThinkingLevel
	domain          string
	port            string
	insecure        bool

	// llmFactory resolves a model ID to an ADK LLM. The Service wraps the
	// LLM in an agentRunner internally and caches both. Nil in tests that
	// inject Runners directly via Config.Runner or Config.RunnerForTier.
	llmFactory LLMFactory
	cacheMu    sync.Mutex
	runners    map[string]Runner
	llms       map[string]adkmodel.LLM

	// tailwindCLI overrides how the per-site CSS compile finds the Tailwind
	// standalone binary. Empty falls back to PATH lookup (tailwindcss, then
	// npx @tailwindcss/cli); when nothing resolves, optimizeCSS is a no-op and
	// pages keep their CDN substrate tags. See css_compile.go.
	tailwindCLI string
	daisyOnce   sync.Once
	daisyDir    string
	daisyErr    error
}

// LLMFactory resolves a model identifier (typically "provider/name") to an
// ADK LLM. The Service wraps the result in an agentRunner so callers that
// just need the raw LLM (caption flow) and callers that need a Runner
// (build/edit/describe flows) share one cached underlying model object.
type LLMFactory func(ctx context.Context, modelID string) (adkmodel.LLM, error)

// Config bundles dependencies for the build service. RecordEdit toggles the
// per-edit transcript capture (enabled by default in production; tests can
// opt out to avoid extra S3 writes). BuildTimeout, when zero, falls back to
// DefaultBuildTimeout. ReasoningEffort, when non-empty, asks the model to
// reason before responding — only useful on reasoning-capable models.
//
// TierMap is the operator-configured per-tier model assignment; the
// Service merges per-build overrides on top before dispatching. Validate
// it before passing in — Service construction will fail otherwise.
//
// LLMFactory resolves model IDs to LLMs lazily, on first use. The Service
// caches the result.
//
// Runner, when set, short-circuits LLMFactory and serves every tier from
// one Runner — used by the happy-path test. RunnerForTier is the finer-
// grained test seam: pin a specific Runner per tier so a test can assert
// that the initial turn went to one stub and the lint retry went to
// another.
type Config struct {
	Store           *store.Store
	TierMap         model.TierMap
	LLMFactory      LLMFactory
	Events          *events.Tracker
	Snapshot        *snapshot.Service
	EditsKeep       int
	RecordEdit      bool
	BuildTimeout    time.Duration
	ReasoningEffort genai.ThinkingLevel
	Runner          Runner
	RunnerForTier   map[model.Tier]Runner
	// Domain / Port / Insecure are passed through to every agentRunner the
	// factory builds so the agent's BuildContext can include a canonical
	// SiteURL for the slug. Empty Domain disables the URL line in the
	// rendered context block (tests and one-off invocations stay clean).
	Domain   string
	Port     string
	Insecure bool

	// TailwindCLI overrides the path to the Tailwind standalone binary used
	// for the post-build per-site CSS compile. Empty falls back to PATH /
	// npx, then to a no-op (pages keep CDN tags). See css_compile.go.
	TailwindCLI string
}

// New is the legacy constructor used by tests that just want a Service
// wired to one stub LLM. Every tier resolves to the same synthesised
// model ID; the directly-injected LLM is pre-populated into the cache so
// no factory invocation ever occurs.
func New(s *store.Store, llm adkmodel.LLM, t *events.Tracker, snap *snapshot.Service) *Service {
	const legacyID = "legacy"
	return &Service{
		store:        s,
		runner:       agentRunner{llm: llm},
		tierMap:      model.TierMap{model.TierAuthor: legacyID},
		events:       t,
		snapshot:     snap,
		recordEdit:   true,
		buildTimeout: DefaultBuildTimeout,
		runners:      map[string]Runner{legacyID: agentRunner{llm: llm}},
		llms:         map[string]adkmodel.LLM{legacyID: llm},
	}
}

// NewWithConfig is the configurable constructor used by cmd/topbanana; New
// stays around for tests and callers that don't care about retention.
func NewWithConfig(cfg Config) *Service {
	timeout := cfg.BuildTimeout
	if timeout <= 0 {
		timeout = DefaultBuildTimeout
	}
	return &Service{
		store:           cfg.Store,
		runner:          cfg.Runner,
		runnersByTier:   cfg.RunnerForTier,
		tierMap:         cfg.TierMap,
		events:          cfg.Events,
		snapshot:        cfg.Snapshot,
		editsKeep:       cfg.EditsKeep,
		recordEdit:      cfg.RecordEdit,
		buildTimeout:    timeout,
		reasoningEffort: cfg.ReasoningEffort,
		domain:          cfg.Domain,
		port:            cfg.Port,
		insecure:        cfg.Insecure,
		tailwindCLI:     cfg.TailwindCLI,
		llmFactory:      cfg.LLMFactory,
		runners:         map[string]Runner{},
		llms:            map[string]adkmodel.LLM{},
	}
}

// runnerForTier resolves the Runner that should serve the given tier for
// this build. Per-build overrides (typically the user's per-tier model
// settings) are merged on top of the operator-configured tier map. Test
// injection wins over factory resolution: a pinned Runner (Config.Runner
// for all tiers, Config.RunnerForTier per-tier) is returned as-is.
//
// Returns the Runner, the resolved model ID (for transcript stamping),
// and any factory error.
func (svc *Service) runnerForTier(ctx context.Context, override model.TierMap, t model.Tier) (Runner, string, error) {
	if r, ok := svc.runnersByTier[t]; ok && r != nil {
		return r, "test-injected", nil
	}
	if svc.runner != nil {
		return svc.runner, "test-injected", nil
	}

	effective := svc.tierMap.Merge(override)
	id := effective.Resolve(t)
	if id == "" {
		return nil, "", fmt.Errorf("build: no model configured for tier %q", t)
	}

	effort := svc.reasoningForTier(t)

	svc.cacheMu.Lock()
	defer svc.cacheMu.Unlock()
	// Runners are keyed by (model ID, reasoning effort): two tiers on the
	// same model but different reasoning levels (author reasons, editor /
	// utility don't) need distinct runner wrappers. The underlying LLM client
	// is still shared via the per-ID llms cache inside resolveLLMLocked, so
	// the factory fires once per model regardless of how many reasoning
	// variants wrap it — the cost the cache exists to avoid.
	key := id + "\x00" + string(effort)
	if r, ok := svc.runners[key]; ok {
		return r, id, nil
	}
	llm, err := svc.resolveLLMLocked(ctx, id)
	if err != nil {
		return nil, id, err
	}
	r := agentRunner{
		llm:             llm,
		reasoningEffort: effort,
		domain:          svc.domain,
		port:            svc.port,
		insecure:        svc.insecure,
	}
	svc.runners[key] = r
	return r, id, nil
}

// reasoningForTier picks the reasoning effort a tier should run at. Only the
// Author tier — open-ended creative generation — benefits from reasoning. The
// Editor tier applies deterministic mechanical lint fixes (LintFixPrompt) and
// the Utility tier writes a short title/description summary; neither gains
// from spending reasoning tokens, so they always run with reasoning off no
// matter what level the operator configured. This is a fixed policy today; if
// it ever needs to be operator-tunable it can grow into a per-tier config map
// the same way model selection did.
func (svc *Service) reasoningForTier(t model.Tier) genai.ThinkingLevel {
	if t == model.TierAuthor {
		return svc.reasoningEffort
	}
	return ""
}

// LLMForTier returns the raw ADK LLM for a tier — the path the caption
// handler uses, since agent.CaptionAsset operates on bytes+mime rather
// than the Runner surface. Same caching as runnerForTier: hits the shared
// per-model-ID cache so identical tiers share one client.
func (svc *Service) LLMForTier(ctx context.Context, override model.TierMap, t model.Tier) (adkmodel.LLM, string, error) {
	effective := svc.tierMap.Merge(override)
	id := effective.Resolve(t)
	if id == "" {
		return nil, "", fmt.Errorf("build: no model configured for tier %q", t)
	}
	svc.cacheMu.Lock()
	defer svc.cacheMu.Unlock()
	llm, err := svc.resolveLLMLocked(ctx, id)
	if err != nil {
		return nil, id, err
	}
	return llm, id, nil
}

// resolveLLMLocked is the inner cache + factory dispatch. Caller must hold
// svc.cacheMu. Returns the cached LLM on hit; invokes svc.llmFactory on
// miss and caches the result before returning.
func (svc *Service) resolveLLMLocked(ctx context.Context, id string) (adkmodel.LLM, error) {
	if llm, ok := svc.llms[id]; ok {
		return llm, nil
	}
	if svc.llmFactory == nil {
		return nil, fmt.Errorf("build: no LLMFactory configured; cannot resolve %q", id)
	}
	llm, err := svc.llmFactory(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("build: resolve LLM for %q: %w", id, err)
	}
	svc.llms[id] = llm
	return llm, nil
}

// Params describes one invocation of Start. LogKey distinguishes build vs.
// edit in slog output. SeedSkeleton (initial builds only) writes the
// template's skeleton files and metadata sidecar before the agent runs.
// Attachments are user-uploaded reference files (markdown or HTML) surfaced
// to the agent as pre-seeded read_attachment calls; one-shot per invocation.
//
// UserPrompt, Page, SelectionLen are forensic context for the edit
// transcript: UserPrompt is the raw user input (Prompt may have been wrapped
// with EditPrompt); Page is the file the visual editor was on; SelectionLen
// is the byte length of the selected HTML fragment. All three are optional.
type Params struct {
	Slug         string
	Prompt       string
	LogKey       string
	Template     *templates.SiteTemplate
	SeedSkeleton bool
	Seeds        []agent.SeedToolCall
	Attachments  []agent.Attachment
	UserPrompt   string
	Page         string
	SelectionLen int
	// OwnerID is the canonical email of the user kicking off the build.
	// Recorded on the initial SiteMeta so every later authorization check
	// has somewhere to look. Empty leaves the sidecar unowned (server
	// startup migration assigns those to the super admin on the next boot).
	OwnerID string

	// Tiers carries the per-user per-tier model overrides for this build.
	// Empty entries fall back to the operator-configured tier map; an empty
	// Tiers entirely means "use the service defaults for every tier".
	//
	// The relint flow uses this to force the whole build onto the Editor
	// tier — the handler promotes the user's Editor model into the Author
	// slot of the override before calling Start.
	Tiers model.TierMap
}

// newRecorder builds the transcript recorder for one run, or nil when
// transcript capture is disabled. It stamps the Author-tier model — the "main"
// creative model for this build (Editor / Utility resolutions are visible in
// the retry / describe transcripts themselves) — and the site template id so a
// transcript records which template's skeleton + prompt addendum shaped it.
func (svc *Service) newRecorder(p Params, authorID string) *editrec.Recorder {
	if !svc.recordEdit {
		return nil
	}
	userPrompt := p.UserPrompt
	if userPrompt == "" {
		userPrompt = p.Prompt
	}
	rec := editrec.New(p.Slug, p.LogKey, userPrompt, p.Page, p.SelectionLen)
	rec.SetModel(authorID, string(svc.reasoningEffort))
	if p.Template != nil {
		rec.SetTemplate(p.Template.ID)
	}
	return rec
}

// Start records the build as in-flight and runs it asynchronously. The
// goroutine emits status events through the tracker; callers render the
// progress page and subscribe via the events handler.
//
// The goroutine walks the build lifecycle step-by-step; splitting it into
// helpers fragments the failure-handling paths without making the flow clearer.
func (svc *Service) Start(p Params) {
	svc.events.Start(p.Slug)

	go func() {
		ctx := context.Background()
		// Resolve one Runner per relevant tier up-front. Each phase of the
		// build lifecycle uses the matching tier:
		//   - Author for the initial agent turn (creative generation).
		//   - Editor for lint-fix retries (mechanical patches).
		//   - Utility for the post-build Describe summary call.
		// Two tiers pointing at the same model share one Runner via the
		// per-model-ID cache.
		authorRunner, authorID, err := svc.runnerForTier(ctx, p.Tiers, model.TierAuthor)
		if err != nil {
			slog.Error(p.LogKey+".runner_resolve_failed", "slug", p.Slug, "tier", "author", "err", err)
			svc.events.Fail(p.Slug, err)
			return
		}
		editorRunner, _, err := svc.runnerForTier(ctx, p.Tiers, model.TierEditor)
		if err != nil {
			slog.Error(p.LogKey+".runner_resolve_failed", "slug", p.Slug, "tier", "editor", "err", err)
			svc.events.Fail(p.Slug, err)
			return
		}
		utilityRunner, _, err := svc.runnerForTier(ctx, p.Tiers, model.TierUtility)
		if err != nil {
			slog.Error(p.LogKey+".runner_resolve_failed", "slug", p.Slug, "tier", "utility", "err", err)
			svc.events.Fail(p.Slug, err)
			return
		}
		if p.SeedSkeleton {
			err := svc.seedTemplate(ctx, p.Slug, p.OwnerID, p.Template)
			if err != nil {
				slog.Error(p.LogKey+".seed_failed", "slug", p.Slug, "template", p.Template.ID, "err", err)
				svc.events.Fail(p.Slug, err)
				return
			}
		}
		// Snapshot post-seed and pre-agent. For initial builds this captures
		// the bare template (restorable to a known-good starting point); for
		// edits it captures the prior agent-built site. Failures are logged
		// but don't block the build — losing undo is better than losing the
		// edit.
		if svc.snapshot != nil {
			_, err := svc.snapshot.Create(ctx, p.Slug, p.LogKey)
			if err != nil {
				slog.Warn(p.LogKey+".snapshot_failed", "slug", p.Slug, "err", err)
			}
		}
		rec := svc.newRecorder(p, authorID)
		err = svc.buildAndLint(ctx, authorRunner, editorRunner, p.Slug, p.Prompt, p.Template, p.Attachments, p.Seeds, !p.SeedSkeleton, rec)
		// Persist the transcript before emitting the terminal SSE event.
		// Consumers (the progress strip, /system, /debug) treat "completed"
		// /"failed" as "you can read everything related to this run now."
		// Emitting the event first leaves a small window where readers see
		// "build done" but the transcript JSON hasn't landed yet — flaked
		// the system-dashboard e2e once and could surprise anyone clicking
		// /debug fast enough.
		if err != nil {
			slog.Error(p.LogKey+".failed", "slug", p.Slug, "err", err)
			if rec != nil {
				rec.Finish(ctx, svc.store, events.StatusFailed, err)
				if svc.editsKeep > 0 {
					editrec.Trim(ctx, svc.store, p.Slug, svc.editsKeep)
				}
			}
			svc.events.Fail(p.Slug, err)
			return
		}
		svc.maybePolish(ctx, editorRunner, p, rec)
		// Compile a minimal, self-contained stylesheet for the finished site
		// and rewrite its pages to link /app.css. Best-effort, exactly like
		// refreshDescription: a failure (no CLI, compile error) logs and moves
		// on. Runs for edits too — Start is the shared entry point.
		svc.OptimizeCSS(ctx, p.Slug)
		svc.refreshDescription(ctx, utilityRunner, p.Slug, p.Prompt)
		slog.Info(p.LogKey+".done", "slug", p.Slug)
		if rec != nil {
			rec.Finish(ctx, svc.store, events.StatusCompleted, nil)
			if svc.editsKeep > 0 {
				editrec.Trim(ctx, svc.store, p.Slug, svc.editsKeep)
			}
		}
		svc.events.Complete(p.Slug)
	}()
}

// Lint runs the standard lint pass against a site. Exposed so callers (like
// a "force re-lint" endpoint) can ask for the same checks the build retry
// loop uses, without invoking the agent.
func (svc *Service) Lint(ctx context.Context, slug string, tmpl *templates.SiteTemplate) []lint.Error {
	return lint.App(ctx, svc.store, slug, tmpl)
}

// AutoFix applies the in-code fixes for deterministically fixable lint errors
// (the kinds in lint.AutoFixers — injecting the /app.css link and the
// responsive viewport meta) and returns the residual errors that still need the
// agent. It reuses the exact path the build
// retry loop runs before falling back to the LLM, so callers like the relint
// endpoint can clear mechanical issues without spending an agent turn (and
// without risking the agent regenerating a page from scratch). Fixes preserve
// existing content.
func (svc *Service) AutoFix(ctx context.Context, slug string, errs []lint.Error) []lint.Error {
	return svc.autoFixLint(ctx, slug, errs)
}

// lintFixGuardrail prefaces every LintFixPrompt with the same edit-in-place
// contract the visual editor uses (see EditPrompt). Without it the agent has
// historically treated a terse "fix this" instruction as license to rewrite a
// whole page from the error text alone — issuing write_file without ever
// reading the file and wiping all unrelated content.
const lintFixGuardrail = "You are fixing lint issues on an existing site. Use read_file to see each affected file's current content first, edit it in place, and do not rewrite pages from scratch or delete content unrelated to the issues listed below."

// polishPrompt is the user-prompt fired by PolishPass — the post-lint
// tightening-up turn. Distilled from the /impeccable polish skill (kept at
// .claude/skills/impeccable/reference/polish.md) and tailored to Top Banana's
// single-page-HTML / DaisyUI substrate; the design-system-discovery,
// TypeScript, real-device-testing, and critique-storage sections from upstream
// don't apply here and were dropped. Re-sync from the upstream if upstream
// adds something we'd want. The body lives in polish_prompt.md.
//
//go:embed polish_prompt.md
var polishPrompt string

// polishEditKeywords are case-insensitive substrings that, when present in an
// edit prompt, cause the polish phase to run after the edit completes. Default
// for edits is to skip polish (a small edit like "change the hero copy" should
// not pay the per-polish-turn cost); users opt in by asking for it.
var polishEditKeywords = []string{"polish", "tighten", "refine", "clean up", "clean it up"}

// shouldPolishEdit returns true when an edit prompt asks for a polish pass.
// Used by Service.Start to gate the polish phase on edits — initial builds
// always polish; edits only polish on opt-in.
func shouldPolishEdit(prompt string) bool {
	p := strings.ToLower(prompt)
	for _, kw := range polishEditKeywords {
		if strings.Contains(p, kw) {
			return true
		}
	}
	return false
}

// maybePolish gates and dispatches the post-lint polish turn. Runs on every
// initial build (SeedSkeleton == true); on edits only when the prompt asks
// for it via shouldPolishEdit, so the typical "change the hero copy" edit
// stays cheap. Best-effort like OptimizeCSS / refreshDescription: any
// failure is logged and the build still completes.
//
// Extracted from Service.Start to keep its cognitive complexity under the
// linter ceiling; the polish-eligibility decision is itself the kind of
// branching that belongs in a named helper.
func (svc *Service) maybePolish(ctx context.Context, editor Runner, p Params, rec *editrec.Recorder) {
	isEdit := !p.SeedSkeleton
	if isEdit && !shouldPolishEdit(p.Prompt) {
		return
	}
	err := svc.PolishPass(ctx, editor, p.Slug, p.Template, p.Attachments, isEdit, rec)
	if err != nil {
		slog.Warn(p.LogKey+".polish_failed", "slug", p.Slug, "err", err)
	}
}

// PolishPass runs one agent turn dedicated to design polish — the post-lint
// "tightening up" phase modelled on the /impeccable polish skill. Uses the
// editor Runner (small/cheap, same one lint-fix retries use) so it never
// touches the author tier's reasoning budget. Best-effort: caller logs any
// returned error and continues; a failed polish never blocks build
// completion.
func (svc *Service) PolishPass(ctx context.Context, editor Runner, slug string, tmpl *templates.SiteTemplate, attachments []agent.Attachment, isEdit bool, rec *editrec.Recorder) error {
	ctx, cancel := context.WithTimeout(ctx, svc.buildTimeout)
	defer cancel()

	emit := func(e events.Event) { svc.events.Emit(slug, e) }
	if rec != nil {
		emit = rec.Wrap(ctx, svc.store, slug, emit)
	}

	emit(events.Event{Type: events.TypeStatus, Status: events.StatusPolishing})

	usage, err := editor.Run(ctx, svc.store, slug, polishPrompt, tmpl, attachments, svc.EditSeeds(ctx, slug, polishPrompt), time.Now(), isEdit, emit, svc.events)
	recordUsage(rec, usage)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("polish timed out after %s", svc.buildTimeout)
		}
		return fmt.Errorf("polish: %w", err)
	}
	return nil
}

// LintFixPrompt formats lint errors as a prompt the agent can act on. Shared
// between the retry loop and any caller (e.g. a force-relint button) that
// wants to kick off a build to fix observed issues.
func LintFixPrompt(errs []lint.Error) string {
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	return lintFixGuardrail + "\n\nFix these issues in the site:\n" + strings.Join(msgs, "\n")
}

// buildAndLint runs the agent then lints with up to maxLintRetries fix-up
// passes when issues are found. author runs the initial creative turn;
// editor handles every lint-fix retry — a deliberate downshift since each
// retry starts a fresh agent session against an already-written site and
// the prompt is a deterministic LintFixPrompt, well within reach of a
// smaller model.
func (svc *Service) buildAndLint(ctx context.Context, author, editor Runner, slug, prompt string, tmpl *templates.SiteTemplate, attachments []agent.Attachment, seeds []agent.SeedToolCall, isEdit bool, rec *editrec.Recorder) error {
	ctx, cancel := context.WithTimeout(ctx, svc.buildTimeout)
	defer cancel()

	emit := func(e events.Event) { svc.events.Emit(slug, e) }
	if rec != nil {
		emit = rec.Wrap(ctx, svc.store, slug, emit)
	}

	// Frozen once at build entry so every retry shows the agent the same
	// calendar day — a long build that crosses midnight should not flip
	// dates mid-flight and invalidate the per-build prefix.
	buildStart := time.Now()

	usage, err := author.Run(ctx, svc.store, slug, prompt, tmpl, attachments, seeds, buildStart, isEdit, emit, svc.events)
	recordUsage(rec, usage)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("build timed out after %s", svc.buildTimeout)
		}
		return err //nolint:wrapcheck // already wrapped at agentRunner boundary
	}

	for attempt := 0; attempt <= maxLintRetries; attempt++ {
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusLinting})
		lintErrs := svc.Lint(ctx, slug, tmpl)
		if len(lintErrs) == 0 {
			return nil
		}

		// Mechanical fixes happen in code before the agent ever sees a
		// retry prompt. A common failure (missing DaisyUI/Tailwind
		// substrate tag) used to cost a full author/editor run with a
		// fresh ~5–7K-token prefix; now it costs an in-process file
		// rewrite and a re-lint.
		residual := svc.autoFixLint(ctx, slug, lintErrs)
		if len(residual) < len(lintErrs) {
			slog.Info("build.autofix",
				"slug", slug,
				"fixed", len(lintErrs)-len(residual),
				"residual", len(residual),
			)
		}
		if len(residual) == 0 {
			continue
		}

		if attempt == maxLintRetries {
			msgs := make([]string, 0, len(residual))
			for _, e := range residual {
				msgs = append(msgs, e.Error())
			}
			return fmt.Errorf("lint errors after %d retries: %s", maxLintRetries, strings.Join(msgs, "; "))
		}

		slog.Info("build.lint_retry", "slug", slug, "attempt", attempt+1, "issues", len(residual))
		emit(events.Event{Type: events.TypeStatus, Status: events.StatusRetry, Message: fmt.Sprintf("fixing %d issue(s)", len(residual))})
		// Seed the retry agent with the affected files' current content (the
		// LintFixPrompt names them) so the fix-up edits in place rather than
		// writing blind.
		fixPrompt := LintFixPrompt(residual)
		usage, err := editor.Run(ctx, svc.store, slug, fixPrompt, tmpl, attachments, svc.EditSeeds(ctx, slug, fixPrompt), buildStart, isEdit, emit, svc.events)
		recordUsage(rec, usage)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return fmt.Errorf("build timed out after %s", svc.buildTimeout)
			}
			return fmt.Errorf("retry: %w", err)
		}
	}

	return nil
}

// recordUsage folds one agent run's token tally into the transcript. Separate
// helper because both the author run and every lint-fix retry feed the same
// recorder, and the agent.Usage → editrec.Usage mapping shouldn't be repeated
// inline. Nil recorder (tests that opt out of transcripts) is a no-op.
func recordUsage(rec *editrec.Recorder, u agent.Usage) {
	rec.AddUsage(editrec.Usage{
		Prompt:     u.Prompt,
		Cached:     u.Cached,
		Candidates: u.Candidates,
		Thoughts:   u.Thoughts,
		ToolUse:    u.ToolUse,
		Total:      u.Total,
		Responses:  u.Responses,
	})
}

// autoFixLint rewrites files in-place for any error whose Kind has a
// deterministic fix (the kinds in lint.AutoFixers), then returns the residual
// errors that still need the agent. Every applicable fixer is applied to a file
// in one read/write pass, so a page missing both the /app.css link and the
// viewport meta is repaired in a single go. Files that also produced a
// KindSuspiciousAttr error are skipped — see the AutoFixDesignSubstrate doc for
// why touching a parser-recovery bug is unsafe.
func (svc *Service) autoFixLint(ctx context.Context, slug string, errs []lint.Error) []lint.Error {
	blocked := map[string]bool{}
	for _, e := range errs {
		if e.Kind == lint.KindSuspiciousAttr {
			blocked[e.File] = true
		}
	}

	// Collect the fixable kinds present per file, deduped — skipping blocked
	// files and any error whose Kind has no registered fixer.
	pending := map[string]map[lint.Kind]bool{}
	for _, e := range errs {
		if blocked[e.File] {
			continue
		}
		if _, ok := lint.AutoFixers[e.Kind]; !ok {
			continue
		}
		if pending[e.File] == nil {
			pending[e.File] = map[lint.Kind]bool{}
		}
		pending[e.File][e.Kind] = true
	}

	fixed := map[string]map[lint.Kind]bool{}
	for file, kinds := range pending {
		obj, err := svc.store.Read(ctx, slug, file)
		if err != nil || obj.Content == "" {
			continue
		}
		content, done := applyAutoFixers(obj.Content, kinds)
		if len(done) == 0 {
			continue
		}
		err = svc.store.Write(ctx, slug, file, content, "text/html; charset=utf-8", nil)
		if err != nil {
			slog.Warn("build.autofix.write_failed", "slug", slug, "file", file, "error", err)
			continue
		}
		fixed[file] = done
	}

	if len(fixed) == 0 {
		return errs
	}

	residual := make([]lint.Error, 0, len(errs))
	for _, e := range errs {
		if fixed[e.File][e.Kind] {
			continue
		}
		residual = append(residual, e)
	}
	return residual
}

// applyAutoFixers chains every registered fixer for the given kinds over
// content, returning the rewritten content and the set of kinds that actually
// changed it. The fixers are idempotent and all inject before </head>, so the
// map-iteration order is irrelevant.
func applyAutoFixers(content string, kinds map[lint.Kind]bool) (string, map[lint.Kind]bool) {
	done := map[lint.Kind]bool{}
	for kind := range kinds {
		out, changed := lint.AutoFixers[kind](content)
		if changed {
			content = out
			done[kind] = true
		}
	}
	return content, done
}

// seedTemplate writes the template's skeleton files (if any) and the
// .topbanana.json sidecar recording the template id. The sidecar lets later
// edits re-apply the same template addendum.
func (svc *Service) seedTemplate(ctx context.Context, slug, ownerID string, tmpl *templates.SiteTemplate) error {
	if tmpl == nil {
		return nil
	}
	for path, content := range tmpl.Skeleton {
		ct := "text/html; charset=utf-8"
		if strings.HasSuffix(path, ".js") {
			ct = "application/javascript; charset=utf-8"
		}
		err := svc.store.Write(ctx, slug, path, content, ct, nil)
		if err != nil {
			return fmt.Errorf("seed %s: %w", path, err)
		}
		slog.Info("template.seed", "slug", slug, "template", tmpl.ID, "path", path)
	}

	return svc.WriteMeta(ctx, slug, SiteMeta{
		Template: tmpl.ID,
		Created:  time.Now().UTC(),
		OwnerID:  ownerID,
	})
}

// refreshDescription asks the LLM for a fresh title + description for the
// site and merges them into the existing sidecar. Best-effort: any failure
// is logged and swallowed so the build still completes — the Available Apps
// page just falls back to showing the slug. runner is the Utility-tier
// Runner resolved at Start: cheap summarisation, separate from the
// creative model that wrote the site.
func (svc *Service) refreshDescription(ctx context.Context, runner Runner, slug, userPrompt string) {
	desc, err := runner.Describe(ctx, svc.store, slug, userPrompt)
	if err != nil {
		slog.Warn("describe.failed", "slug", slug, "err", err)
		return
	}
	meta := svc.ReadMeta(ctx, slug)
	meta.Title = desc.Title
	meta.Description = desc.Description
	err = svc.WriteMeta(ctx, slug, meta)
	if err != nil {
		slog.Warn("describe.write_failed", "slug", slug, "err", err)
	}
}

// WriteMeta persists the per-site sidecar.
func (svc *Service) WriteMeta(ctx context.Context, slug string, meta SiteMeta) error {
	body, err := json.Marshal(meta)
	if err != nil {
		return fmt.Errorf("encode site meta: %w", err)
	}
	err = svc.store.Write(ctx, slug, MetaFile, string(body), "application/json", nil)
	if err != nil {
		return fmt.Errorf("write site meta: %w", err)
	}
	return nil
}

// NormalizeDomain lowercases and strips an optional port from a user-entered
// host. Returns an error for empty input or anything that isn't a plausible
// hostname (catches obvious typos like trailing slashes or schemes).
func NormalizeDomain(raw string) (string, error) {
	h := strings.ToLower(strings.TrimSpace(raw))
	if i := strings.LastIndex(h, ":"); i != -1 {
		h = h[:i]
	}
	if h == "" {
		return "", errors.New("empty domain")
	}
	if strings.ContainsAny(h, "/ \t\r\n") || strings.Contains(h, "://") {
		return "", fmt.Errorf("invalid domain %q", raw)
	}
	if !strings.Contains(h, ".") {
		return "", fmt.Errorf("domain %q must contain a dot", raw)
	}
	return h, nil
}

// ReadMeta returns the recorded sidecar for an existing site, or a zero value
// if the sidecar is missing (older sites pre-date templates). Falls through
// to legacyMetaFiles in order for sites created before a rebrand.
func (svc *Service) ReadMeta(ctx context.Context, slug string) SiteMeta {
	obj, err := svc.store.Read(ctx, slug, MetaFile)
	if err != nil {
		slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	for _, legacy := range legacyMetaFiles {
		if obj.Content != "" {
			break
		}
		obj, err = svc.store.Read(ctx, slug, legacy)
		if err != nil {
			slog.Warn("site_meta.read_failed", "slug", slug, "err", err)
			return SiteMeta{}
		}
	}
	if obj.Content == "" {
		return SiteMeta{}
	}
	var m SiteMeta
	err = json.Unmarshal([]byte(obj.Content), &m)
	if err != nil {
		slog.Warn("site_meta.decode_failed", "slug", slug, "err", err)
		return SiteMeta{}
	}
	return m
}
