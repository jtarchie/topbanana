// Package agent wires the ADK runner, tools, and system prompt for a single
// build. It also provides the vision-captioning entrypoint used during asset
// uploads, since both consume the configured LLM model.
package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"

	"github.com/achetronic/adk-utils-go/plugin/contextguard"
	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
	"github.com/jtarchie/topbanana/internal/textedit"
)

//go:embed agent_prompt.md
var systemPrompt string

//go:embed functions_prompt.md
var functionsPrompt string

// SeedToolCall is a synthetic tool-call/response pair the caller can
// pre-populate in the agent's session. The model sees these as if it had
// already issued the call and received the response, so we skip round-trips
// for things we already know (the file list, the content of pages the user
// named in the prompt).
type SeedToolCall struct {
	Name     string
	Args     map[string]any
	Response map[string]any
}

// Attachment is a reference file (markdown or HTML) the user attached to a
// build or edit submission. Lifetime is the single agent run: each attachment
// becomes a seeded read_attachment(name) call/response so the model sees the
// content in history without spending a turn on it. Name is the sanitized
// basename; Content is the raw text.
type Attachment struct {
	Name    string
	Content string
}

type readAttachmentArgs struct {
	Name string `json:"name"`
}

type readAttachmentResult struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

type readExampleArgs struct {
	Name string `json:"name"`
}

type readExampleResult struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

// Tool results surface errors as data (Error field) rather than as a Go
// error: this lets the model see the failure in the tool response and recover
// (e.g. retry with a different path) instead of aborting the run.

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type writeFileResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	Hints string `json:"hints,omitempty"`
}

type readFileArgs struct {
	Path string `json:"path"`
	// StartLine and EndLine are optional 1-indexed inclusive line bounds. Zero
	// means "from the beginning" or "through the end" respectively, so the
	// zero-value behaviour (both fields unset) returns the whole file — keeping
	// backward compatibility with seeded responses in internal/build/edit.go.
	StartLine int `json:"start_line,omitempty"`
	EndLine   int `json:"end_line,omitempty"`
}

type readFileResult struct {
	Content    string `json:"content"`
	TotalLines int    `json:"total_lines,omitempty"`
	Error      string `json:"error,omitempty"`
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type editFileResult struct {
	OK           bool   `json:"ok"`
	Replacements int    `json:"replacements,omitempty"`
	Note         string `json:"note,omitempty"`
	Error        string `json:"error,omitempty"`
	Hints        string `json:"hints,omitempty"`
}

type replaceLinesArgs struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	NewText   string `json:"new_text"`
}

type insertAtLineArgs struct {
	Path      string `json:"path"`
	AfterLine int    `json:"after_line"`
	Content   string `json:"content"`
}

type grepFilesArgs struct {
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results,omitempty"`
}

type grepMatch struct {
	Path       string `json:"path"`
	LineNumber int    `json:"line_number"`
	Snippet    string `json:"snippet"`
}

type grepFilesResult struct {
	Matches      []grepMatch `json:"matches"`
	TotalMatches int         `json:"total_matches"`
	Truncated    bool        `json:"truncated,omitempty"`
	Error        string      `json:"error,omitempty"`
}

type listFilesResult struct {
	Files []string `json:"files"`
	Error string   `json:"error,omitempty"`
}

type assetEntry struct {
	Path        string `json:"path"`
	Alt         string `json:"alt,omitempty"`
	Description string `json:"description,omitempty"`
}

type listAssetsResult struct {
	Assets []assetEntry `json:"assets"`
	Error  string       `json:"error,omitempty"`
}

type writeFunctionArgs struct {
	Name   string `json:"name"`
	Source string `json:"source"`
}

type writeFunctionResult struct {
	OK    bool   `json:"ok"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

type readFunctionArgs struct {
	Name string `json:"name"`
}

type readFunctionResult struct {
	Source string `json:"source"`
	Error  string `json:"error,omitempty"`
}

type listFunctionsResult struct {
	Functions []string `json:"functions"`
	Error     string   `json:"error,omitempty"`
}

type editFunctionArgs struct {
	Name       string `json:"name"`
	OldText    string `json:"old_text"`
	NewText    string `json:"new_text"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

type editFunctionResult struct {
	OK           bool   `json:"ok"`
	Path         string `json:"path,omitempty"`
	Replacements int    `json:"replacements,omitempty"`
	Note         string `json:"note,omitempty"`
	Error        string `json:"error,omitempty"`
}

type deleteFunctionArgs struct {
	Name string `json:"name"`
}

type deleteFunctionResult struct {
	OK    bool   `json:"ok"`
	Path  string `json:"path,omitempty"`
	Error string `json:"error,omitempty"`
}

type askUserArgs struct {
	Question       string   `json:"question"`
	Recommendation string   `json:"recommendation"`
	Why            string   `json:"why"`
	Options        []string `json:"options,omitempty"`
}

type askUserResult struct {
	Answer string `json:"answer"`
	Source string `json:"source"` // "user" | "recommendation_timeout" | "limit_reached"
	Error  string `json:"error,omitempty"`
}

// Run invokes the agent against the given slug. emit may be nil. attachments
// are inlined into the session as seeded read_attachment(name) call/response
// pairs ahead of the caller-supplied seeds. reasoningEffort, when non-empty,
// asks the model to spend reasoning tokens before each response — supported
// by Gemini 2.5/3 Flash + Pro, Claude Sonnet/Opus/Haiku, Qwen Plus, GPT-5,
// DeepSeek V3.1+ and other reasoning-capable models on OpenRouter.
// BuildContext carries the per-build meta facts the agent needs to pin its
// output to reality: today's date (for copyright years, "last updated"
// footers, blog dates that don't drift back to the model's training year),
// the site's canonical slug + URL (for og:url, self-links, and so the
// agent has a single source of truth for the host it is generating
// against), and whether this is the initial build or a follow-up edit
// (drives the "rewrite vs surgical edit" framing in the instruction).
type BuildContext struct {
	Now     time.Time
	Slug    string
	SiteURL string
	IsEdit  bool
}

// Usage accumulates the token counts the model reports across one agent run.
// Gemini-style usage metadata reports a per-response total, so we sum across
// the model-response events of the run. Cached comes straight from the
// provider's prompt cache, which makes CacheHitRatio a direct readout of
// whether the cache-stable instruction prefix is actually being reused — the
// single most useful signal for telling if the prompt-ordering work is
// paying off. A run that errors mid-flight still returns whatever was spent
// up to that point.
type Usage struct {
	Prompt     int64 // total prompt tokens (includes the cached portion)
	Cached     int64 // prompt tokens served from the provider's cache
	Candidates int64 // generated output tokens
	Thoughts   int64 // reasoning tokens (0 when reasoning is off)
	ToolUse    int64 // tokens billed for tool-result inputs
	Total      int64 // provider-reported grand total across the run
	Responses  int   // model responses that carried usage metadata
}

func (u Usage) add(m *genai.GenerateContentResponseUsageMetadata) Usage {
	if m == nil {
		return u
	}
	u.Prompt += int64(m.PromptTokenCount)
	u.Cached += int64(m.CachedContentTokenCount)
	u.Candidates += int64(m.CandidatesTokenCount)
	u.Thoughts += int64(m.ThoughtsTokenCount)
	u.ToolUse += int64(m.ToolUsePromptTokenCount)
	u.Total += int64(m.TotalTokenCount)
	u.Responses++
	return u
}

// CacheHitRatio is the fraction of prompt tokens served from cache. Zero when
// nothing was prompted yet (avoids a divide-by-zero on empty runs).
func (u Usage) CacheHitRatio() float64 {
	if u.Prompt == 0 {
		return 0
	}
	return float64(u.Cached) / float64(u.Prompt)
}

func Run(ctx context.Context, llm adkmodel.LLM, s *store.Store, slug, prompt string, tmpl *templates.SiteTemplate, attachments []Attachment, seeds []SeedToolCall, reasoningEffort genai.ThinkingLevel, bctx BuildContext, emit func(events.Event), tracker *events.Tracker) (Usage, error) {
	if emit == nil {
		emit = func(events.Event) {}
	}

	state := newBuildState()

	// contextcheck flags this because Run has a ctx, but the tools fire later
	// under per-invocation contexts from the runner; passing ctx would be wrong.
	tools, err := buildAgentTools(s, slug, tmpl, attachments, emit, state, tracker) //nolint:contextcheck
	if err != nil {
		return Usage{}, err
	}

	cfg := llmagent.Config{
		Name:        "html-builder",
		Description: "Builds static HTML apps from a prompt",
		Instruction: buildInstruction(tmpl, attachments, bctx),
		Model:       llm,
		Tools:       tools,
	}
	// Parallel tool calls are intentionally left to provider defaults.
	// genai.GenerateContentConfig has no parallel-tool-calls field, and the
	// adk-utils-go OpenAI/Anthropic adapters expose no hook for OpenAI's
	// parallel_tool_calls or Anthropic's disable_parallel_tool_use. Both
	// providers default to *enabled*, and ADK's runner fans concurrent calls
	// into goroutines (base_flow.handleFunctionCalls), so the model can batch
	// freely. buildState.writeMu serializes the read-modify-write tools so
	// that fan-out doesn't lose work at S3.
	if reasoningEffort != "" {
		cfg.GenerateContentConfig = &genai.GenerateContentConfig{
			ThinkingConfig: &genai.ThinkingConfig{ThinkingLevel: reasoningEffort},
		}
	}

	a, err := llmagent.New(cfg)
	if err != nil {
		return Usage{}, fmt.Errorf("create agent: %w", err)
	}

	// Examples first (aspirational references the model should see before
	// anything else), then skeletons (so the model arrives knowing what's
	// already on disk without spending tool turns to discover it), then
	// attachments (the user's authoritative content), then caller-supplied
	// seeds (per-page reads, list_files, etc).
	allSeeds := append(ExampleSeeds(tmpl), SkeletonSeeds(tmpl)...)
	allSeeds = append(allSeeds, AttachmentSeeds(attachments)...)
	allSeeds = append(allSeeds, seeds...)

	sessSvc := session.InMemoryService()
	sess, err := seedSession(ctx, sessSvc, slug, allSeeds)
	if err != nil {
		return Usage{}, err
	}

	// History compaction: cap the conversation at ~20 entries so a long
	// build does not replay an ever-growing history on every turn. The
	// sliding-window strategy is turn-based (no tokenizer needed), uses
	// this agent's own LLM to produce the summary, and only fires when
	// the cap is exceeded — short builds pay nothing. CrushRegistry
	// ships with the upstream package and provides context-window
	// metadata for known model IDs, with sane fallbacks otherwise.
	guard := contextguard.New(contextguard.NewCrushRegistry())
	guard.Add(cfg.Name, llm, contextguard.WithSlidingWindow(20))

	r, err := runner.New(runner.Config{
		AppName:           "topbanana",
		Agent:             a,
		SessionService:    sessSvc,
		AutoCreateSession: false,
		PluginConfig:      guard.PluginConfig(),
	})
	if err != nil {
		return Usage{}, fmt.Errorf("create runner: %w", err)
	}

	userMsg := &genai.Content{
		Parts: []*genai.Part{genai.NewPartFromText(prompt)},
		Role:  "user",
	}

	// usage sums the per-response token counts the provider reports. Logged
	// once on the way out (any exit point — clean finish, iteration cap, or
	// mid-run error) so a forensic reader always sees what the run actually
	// cost, even when it bailed.
	var usage Usage
	defer func() {
		slog.Info("agent.usage", "slug", slug,
			"prompt", usage.Prompt, "cached", usage.Cached,
			"candidates", usage.Candidates, "thoughts", usage.Thoughts,
			"tool_use", usage.ToolUse, "total", usage.Total,
			"responses", usage.Responses, "cache_hit_ratio", usage.CacheHitRatio())
	}()

	for event, err := range r.Run(ctx, sess.UserID(), sess.ID(), userMsg, agent.RunConfig{}) {
		if err != nil {
			return usage, fmt.Errorf("agent error: %w", err)
		}
		if event != nil {
			usage = usage.add(event.UsageMetadata)
		}
		iters := state.iters.Add(1)
		if iters > maxAgentIterations {
			slog.Warn("agent.iteration_cap", "slug", slug, "iterations", iters, "max", maxAgentIterations)
			return usage, fmt.Errorf("agent exceeded %d iterations without finalizing", maxAgentIterations)
		}
		// Surface multi-tool-call batches so we can confirm the model is
		// actually fanning out and that the writeMu serialization isn't
		// being exercised by an empty hypothetical. Single-call turns stay
		// quiet — that's the common case and not interesting.
		if n := countFunctionCalls(event); n > 1 {
			slog.Info("agent.parallel_tool_calls", "slug", slug, "count", n)
		}
		if event != nil && event.IsFinalResponse() {
			slog.Info("agent.done", "slug", slug, "iterations", iters)
			break
		}
	}

	return usage, nil
}

// seedSession creates a fresh session for the given slug and pre-populates it
// with synthetic tool-call/response pairs.
func seedSession(ctx context.Context, sessSvc session.Service, slug string, seeds []SeedToolCall) (session.Session, error) {
	createResp, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "topbanana",
		UserID:    slug,
		SessionID: slug,
	})
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	sess := createResp.Session

	for i, s := range seeds {
		invID := fmt.Sprintf("seed-%d", i)
		callID := fmt.Sprintf("seed-call-%d", i)

		callPart := genai.NewPartFromFunctionCall(s.Name, s.Args)
		callPart.FunctionCall.ID = callID
		callEv := session.NewEvent(invID)
		callEv.Author = "html-builder"
		callEv.Timestamp = time.Now()
		callEv.LLMResponse = adkmodel.LLMResponse{Content: &genai.Content{
			Role:  "model",
			Parts: []*genai.Part{callPart},
		}}
		err := sessSvc.AppendEvent(ctx, sess, callEv)
		if err != nil {
			return nil, fmt.Errorf("seed call %s: %w", s.Name, err)
		}

		respPart := genai.NewPartFromFunctionResponse(s.Name, s.Response)
		respPart.FunctionResponse.ID = callID
		respEv := session.NewEvent(invID)
		respEv.Author = "user"
		respEv.Timestamp = time.Now()
		respEv.LLMResponse = adkmodel.LLMResponse{Content: &genai.Content{
			Role:  "user",
			Parts: []*genai.Part{respPart},
		}}
		err = sessSvc.AppendEvent(ctx, sess, respEv)
		if err != nil {
			return nil, fmt.Errorf("seed response %s: %w", s.Name, err)
		}
	}

	return sess, nil
}

// emitter wraps the emit callback with the tool name so call sites stay short.
type emitter struct {
	emit func(events.Event)
	tool string
}

func (e emitter) start(path string) {
	e.emit(events.Event{Type: events.TypeTool, Tool: e.tool, Phase: events.PhaseStart, Path: path})
}

func (e emitter) done(path string) {
	e.emit(events.Event{Type: events.TypeTool, Tool: e.tool, Phase: events.PhaseDone, Path: path})
}

func (e emitter) fail(path string, err error) {
	e.emit(events.Event{Type: events.TypeTool, Tool: e.tool, Phase: events.PhaseError, Path: path, Message: err.Error()})
}

// buildAgentTools constructs the tools the agent uses against a single site.
// agent.ToolContext chains down to context.Context via interface embedding, so it's
// passed to store methods directly. Tool callbacks fire later from the runner
// with their own per-invocation context — that is the correct one to forward
// (contextcheck objects to this but is wrong).
//
// The function-authoring tools (write_function, read_function, list_functions)
// are only registered when the template opts in via EnablesFunctions. Older
// brochure templates see no behavioural change.
func buildAgentTools(s *store.Store, slug string, tmpl *templates.SiteTemplate, attachments []Attachment, emit func(events.Event), state *buildState, tracker *events.Tracker) ([]tool.Tool, error) {
	// guardedBuilders read/update buildState (anti-loop ring + iteration
	// counter for the budget hint); plain builders don't need it.
	guardedBuilders := []func(*store.Store, string, func(events.Event), *buildState) (tool.Tool, error){
		newWriteFileTool,
		newEditFileTool,
		newReplaceLinesTool,
		newInsertAtLineTool,
	}
	plainBuilders := []func(*store.Store, string, func(events.Event)) (tool.Tool, error){
		newReadFileTool,
		newListFilesTool,
		newGrepFilesTool,
		newListAssetsTool,
	}
	tools := make([]tool.Tool, 0, len(guardedBuilders)+len(plainBuilders)+6)
	for _, b := range guardedBuilders {
		t, err := b(s, slug, emit, state)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	for _, b := range plainBuilders {
		t, err := b(s, slug, emit)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	attTool, err := newReadAttachmentTool(attachments, emit)
	if err != nil {
		return nil, err
	}
	tools = append(tools, attTool)
	exTool, err := newReadExampleTool(tmpl, emit)
	if err != nil {
		return nil, err
	}
	tools = append(tools, exTool)
	refTool, err := newFetchReferenceTool(emit)
	if err != nil {
		return nil, err
	}
	tools = append(tools, refTool)
	if tmpl != nil && tmpl.EnablesFunctions {
		fnGuardedBuilders := []func(*store.Store, string, func(events.Event), *buildState) (tool.Tool, error){
			newWriteFunctionTool,
			newEditFunctionTool,
			newDeleteFunctionTool,
		}
		fnPlainBuilders := []func(*store.Store, string, func(events.Event)) (tool.Tool, error){
			newReadFunctionTool,
			newListFunctionsTool,
		}
		for _, b := range fnGuardedBuilders {
			t, err := b(s, slug, emit, state)
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
		for _, b := range fnPlainBuilders {
			t, err := b(s, slug, emit)
			if err != nil {
				return nil, err
			}
			tools = append(tools, t)
		}
	}
	return appendAskUserTool(tools, slug, tracker, emit, state)
}

func appendAskUserTool(tools []tool.Tool, slug string, tracker *events.Tracker, emit func(events.Event), state *buildState) ([]tool.Tool, error) {
	if tracker == nil {
		return tools, nil
	}
	askTool, err := newAskUserTool(slug, tracker, emit, state)
	if err != nil {
		return nil, err
	}
	return append(tools, askTool), nil
}

// newReadAttachmentTool exposes user-attached files (markdown or HTML) to the
// agent. Attachments are passed in by value (already snapshotted at request
// time) so the tool closure is safe for concurrent reads across runs. Always
// registered — when no files are attached the tool reports an error in the
// result rather than going missing, which keeps the tool surface uniform
// regardless of how the run was invoked.
func newReadAttachmentTool(attachments []Attachment, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "read_attachment"}
	index := make(map[string]string, len(attachments))
	names := make([]string, 0, len(attachments))
	for _, a := range attachments {
		if _, dup := index[a.Name]; dup {
			continue
		}
		index[a.Name] = a.Content
		names = append(names, a.Name)
	}
	available := strings.Join(names, ", ")
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "read_attachment",
			Description: "Re-read a user-uploaded markdown or HTML attachment. The full set is pre-loaded into history; call only when you need a second look.",
		},
		func(_ agent.ToolContext, args readAttachmentArgs) (readAttachmentResult, error) {
			em.start(args.Name)
			content, ok := index[args.Name]
			if !ok {
				msg := "no files attached on this run"
				if available != "" {
					msg = fmt.Sprintf("no attachment named %q (available: %s)", args.Name, available)
				}
				err := errors.New(msg)
				em.fail(args.Name, err)
				return readAttachmentResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_attachment", "name", args.Name, "length", len(content))
			em.done(args.Name)
			return readAttachmentResult{Content: NumberLines(content, 1)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_attachment tool: %w", err)
	}
	return t, nil
}

// AttachmentSeeds returns one synthetic read_attachment(name) call/response
// per attachment. Prepended to caller-supplied seeds so the model sees
// reference material before any per-page reads.
func AttachmentSeeds(attachments []Attachment) []SeedToolCall {
	if len(attachments) == 0 {
		return nil
	}
	seeds := make([]SeedToolCall, 0, len(attachments))
	for _, a := range attachments {
		seeds = append(seeds, SeedToolCall{
			Name:     "read_attachment",
			Args:     map[string]any{"name": a.Name},
			Response: map[string]any{"content": NumberLines(a.Content, 1)},
		})
	}
	return seeds
}

// newReadExampleTool exposes the template's aspirational exemplar HTML files
// (under sites/{id}/examples/) to the agent. Same shape as read_attachment but
// the content comes from the embedded template registry — these aren't the
// user's authoritative copy, they're "what good looks like" references the
// model should imitate aesthetically. Always registered: when a template
// ships no examples the tool reports an error in the result rather than
// disappearing.
func newReadExampleTool(tmpl *templates.SiteTemplate, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "read_example"}
	index := map[string]string{}
	names := []string{}
	if tmpl != nil {
		for n, c := range tmpl.Examples {
			index[n] = c
			names = append(names, n)
		}
		sort.Strings(names)
	}
	available := strings.Join(names, ", ")
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "read_example",
			Description: "Re-read an aspirational reference HTML page for this template. Inspiration only — never copy markup verbatim. Pre-loaded into history; call only when you need a second look.",
		},
		func(_ agent.ToolContext, args readExampleArgs) (readExampleResult, error) {
			em.start(args.Name)
			content, ok := index[args.Name]
			if !ok {
				msg := "no examples available for this template"
				if available != "" {
					msg = fmt.Sprintf("no example named %q (available: %s)", args.Name, available)
				}
				err := errors.New(msg)
				em.fail(args.Name, err)
				return readExampleResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_example", "name", args.Name, "length", len(content))
			em.done(args.Name)
			return readExampleResult{Content: NumberLines(content, 1)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_example tool: %w", err)
	}
	return t, nil
}

// ExampleSeeds returns one synthetic read_example(name) call/response per
// example file shipped by the template. Prepended to attachments and other
// seeds so the model sees aesthetic references first — this is the "few-shot
// what good looks like" path. Returns nil when the template has no examples,
// so older brochure templates pay no token cost.
// SkeletonSeeds emits the synthetic tool-call/response pairs the agent
// would otherwise have to run itself to discover the files seedTemplate
// already wrote to S3: one list_files (HTML pages), one list_functions
// (handler names) when any are present, and one read_file / read_function
// per skeleton file. Without these the first build turns are spent on
// pure discovery — list, then read each path — and the model pays
// for that exchange on every subsequent turn because the history is
// replayed in full.
func SkeletonSeeds(tmpl *templates.SiteTemplate) []SeedToolCall {
	if tmpl == nil || len(tmpl.Skeleton) == 0 {
		return nil
	}

	var htmlPaths, jsPaths []string
	for p := range tmpl.Skeleton {
		switch {
		case strings.HasSuffix(p, ".html"):
			htmlPaths = append(htmlPaths, p)
		case strings.HasPrefix(p, functionsDir) && strings.HasSuffix(p, jsExt):
			jsPaths = append(jsPaths, p)
		}
	}
	sort.Strings(htmlPaths)
	sort.Strings(jsPaths)

	seeds := make([]SeedToolCall, 0, 2+len(htmlPaths)+len(jsPaths))

	if len(htmlPaths) > 0 {
		seeds = append(seeds, SeedToolCall{
			Name:     "list_files",
			Args:     map[string]any{},
			Response: map[string]any{"files": htmlPaths},
		})
		for _, p := range htmlPaths {
			content := tmpl.Skeleton[p]
			seeds = append(seeds, SeedToolCall{
				Name: "read_file",
				Args: map[string]any{"path": p},
				Response: map[string]any{
					"content":     NumberLines(content, 1),
					"total_lines": lineCount(content),
				},
			})
		}
	}

	if len(jsPaths) > 0 {
		fnNames := make([]string, 0, len(jsPaths))
		for _, p := range jsPaths {
			fnNames = append(fnNames, strings.TrimSuffix(strings.TrimPrefix(p, functionsDir), jsExt))
		}
		seeds = append(seeds, SeedToolCall{
			Name:     "list_functions",
			Args:     map[string]any{},
			Response: map[string]any{"functions": fnNames},
		})
		for i, p := range jsPaths {
			seeds = append(seeds, SeedToolCall{
				Name:     "read_function",
				Args:     map[string]any{"name": fnNames[i]},
				Response: map[string]any{"source": tmpl.Skeleton[p]},
			})
		}
	}

	return seeds
}

// lineCount returns the 1-based number of lines NumberLines would emit for
// the given content (matches the convention read_file reports). An empty
// string is 0 lines; otherwise a trailing newline does not add a phantom
// final line.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	n := strings.Count(s, "\n")
	if !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

// maxSeededExamples caps how many examples we pre-load into the agent's
// session, even if the template ships more. Each example is several KB of
// HTML that the agent pays for on every turn (history is replayed), so
// beyond the first couple the marginal taste-information per token gets
// thin. The cap is deliberately permissive — today's templates all ship
// exactly two, so this only kicks in if a contributor adds a third. The
// remaining examples remain reachable via the read_example tool.
const maxSeededExamples = 2

func ExampleSeeds(tmpl *templates.SiteTemplate) []SeedToolCall {
	if tmpl == nil || len(tmpl.Examples) == 0 {
		return nil
	}
	names := make([]string, 0, len(tmpl.Examples))
	for n := range tmpl.Examples {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > maxSeededExamples {
		names = names[:maxSeededExamples]
	}
	seeds := make([]SeedToolCall, 0, len(names))
	for _, n := range names {
		seeds = append(seeds, SeedToolCall{
			Name:     "read_example",
			Args:     map[string]any{"name": n},
			Response: map[string]any{"content": NumberLines(tmpl.Examples[n], 1)},
		})
	}
	return seeds
}

func newWriteFileTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "write_file"}
	t, err := functiontool.New(
		functiontool.Config{Name: "write_file", Description: "Write content to an HTML file"},
		func(tctx agent.ToolContext, args writeFileArgs) (writeFileResult, error) {
			em.start(args.Path)
			err := validateHTMLPath(args.Path)
			if err != nil {
				em.fail(args.Path, err)
				return writeFileResult{Error: err.Error()}, nil
			}
			if len(args.Content) > maxHTMLFileBytes {
				err = fmt.Errorf("content too large: %d bytes (max %d)", len(args.Content), maxHTMLFileBytes)
				em.fail(args.Path, err)
				return writeFileResult{Error: err.Error()}, nil
			}
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			// File-count cap: only block when this path would create a *new*
			// HTML file beyond the limit. Overwrites of existing files are
			// always allowed. List failures don't block the write — we'd
			// rather risk an extra file than fail a legitimate edit because
			// of a transient S3 hiccup.
			files, listErr := s.List(tctx, slug)
			htmlCount, exists := -1, false
			if listErr == nil {
				htmlCount = 0
				for _, f := range files {
					if f == args.Path {
						exists = true
					}
					if strings.HasSuffix(f, ".html") {
						htmlCount++
					}
				}
				if !exists && htmlCount >= maxHTMLFiles {
					err = fmt.Errorf("site has reached the %d HTML file limit", maxHTMLFiles)
					em.fail(args.Path, err)
					return writeFileResult{Error: err.Error(), Hints: budgetHint(htmlCount, state)}, nil
				}
			} else {
				slog.Warn("agent.write_file.list", "slug", slug, "err", listErr)
			}
			err = state.guard.Allow(toolSignature("write_file", args.Path, args.Content))
			if err != nil {
				em.fail(args.Path, err)
				return writeFileResult{Error: err.Error()}, nil
			}
			err = s.Write(tctx, slug, args.Path, args.Content, "text/html; charset=utf-8", nil)
			if err != nil {
				slog.Warn("agent.write_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return writeFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.write_file", "slug", slug, "path", args.Path, "length", len(args.Content))
			em.done(args.Path)
			postCount := htmlCount
			if postCount >= 0 && !exists {
				postCount++
			}
			return writeFileResult{OK: true, Hints: budgetHint(postCount, state)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create write_file tool: %w", err)
	}
	return t, nil
}

func newReadFileTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "read_file"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "read_file",
			Description: "Read an HTML file. Pass start_line/end_line (1-indexed inclusive) for a slice. Lines come back prefixed with their 1-indexed number and a tab — strip that annotation before passing text back.",
		},
		func(tctx agent.ToolContext, args readFileArgs) (readFileResult, error) {
			em.start(args.Path)
			err := validateHTMLPath(args.Path)
			if err != nil {
				em.fail(args.Path, err)
				return readFileResult{Error: err.Error()}, nil
			}
			obj, err := s.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.read_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return readFileResult{Error: err.Error()}, nil
			}
			content, total, sliceErr := sliceLines(obj.Content, args.StartLine, args.EndLine)
			if sliceErr != nil {
				em.fail(args.Path, sliceErr)
				return readFileResult{Error: sliceErr.Error(), TotalLines: total}, nil
			}
			numbered := NumberLines(content, max(args.StartLine, 1))
			slog.Info("agent.read_file", "slug", slug, "path", args.Path,
				"length", len(content), "total_lines", total,
				"start_line", args.StartLine, "end_line", args.EndLine)
			em.done(args.Path)
			return readFileResult{Content: numbered, TotalLines: total}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_file tool: %w", err)
	}
	return t, nil
}

// NumberLines prefixes every line with its 1-indexed line number (cat -n
// style). Kept as an exported alias so internal/build's seeded read_file
// responses can use the same transform; the implementation lives in
// internal/textedit and is shared with the MCP editing surface.
func NumberLines(content string, startOffset int) string {
	return textedit.NumberLines(content, startOffset)
}

// sliceLines returns a 1-indexed-inclusive slice of content plus the full
// line count. Delegates to internal/textedit so the agent and MCP slice
// identically.
func sliceLines(content string, start, end int) (string, int, error) {
	return textedit.SliceLines(content, start, end)
}

func newEditFileTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "edit_file"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "edit_file",
			Description: "Surgical edit on an HTML file: old_text must byte-match and be unique unless replace_all=true. Prefer this over rewriting the whole file.",
		},
		func(tctx agent.ToolContext, args editFileArgs) (editFileResult, error) {
			em.start(args.Path)
			pathErr := validateHTMLPath(args.Path)
			if pathErr != nil {
				em.fail(args.Path, pathErr)
				return editFileResult{Error: pathErr.Error()}, nil
			}
			if args.OldText == "" {
				em.fail(args.Path, errors.New("old_text required"))
				return editFileResult{Error: "old_text is required"}, nil
			}
			if args.OldText == args.NewText {
				// No-op is not an error: the model occasionally submits
				// identical strings while reasoning about a fix. Returning
				// success with replacements=0 + a note lets the loop continue
				// instead of burning a lint-retry on a "no-op edit" failure.
				em.done(args.Path)
				return editFileResult{
					OK:   true,
					Note: "old_text and new_text are identical; no change made",
				}, nil
			}
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			obj, err := s.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.edit_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			if obj.Content == "" {
				em.fail(args.Path, errors.New("file not found"))
				return editFileResult{Error: "file not found: " + args.Path}, nil
			}
			updated, count, note, applyErr := applyEdit(obj.Content, args.OldText, args.NewText, args.ReplaceAll)
			if applyErr != nil {
				em.fail(args.Path, applyErr)
				return editFileResult{Error: applyErr.Error()}, nil
			}
			if len(updated) > maxHTMLFileBytes {
				sizeErr := fmt.Errorf("content too large after edit: %d bytes (max %d)", len(updated), maxHTMLFileBytes)
				em.fail(args.Path, sizeErr)
				return editFileResult{Error: sizeErr.Error()}, nil
			}
			guardErr := state.guard.Allow(toolSignature("edit_file", args.Path, args.OldText, args.NewText))
			if guardErr != nil {
				em.fail(args.Path, guardErr)
				return editFileResult{Error: guardErr.Error()}, nil
			}
			contentType := obj.ContentType
			if contentType == "" {
				contentType = "text/html; charset=utf-8"
			}
			err = s.Write(tctx, slug, args.Path, updated, contentType, obj.Metadata)
			if err != nil {
				slog.Warn("agent.edit_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.edit_file", "slug", slug, "path", args.Path,
				"old_len", len(args.OldText), "new_len", len(args.NewText), "replacements", count)
			em.done(args.Path)
			return editFileResult{OK: true, Replacements: count, Note: note, Hints: budgetHint(-1, state)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create edit_file tool: %w", err)
	}
	return t, nil
}

// newReplaceLinesTool returns a tool that replaces a 1-indexed inclusive
// line range with new_text. Pairs with read_file's line-numbered output so
// the agent can sidestep whitespace-matching entirely when it already knows
// the exact lines to swap. Pass empty new_text to delete the range.
func newReplaceLinesTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "replace_lines"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "replace_lines",
			Description: "Replace lines start_line..end_line (1-indexed inclusive) in an HTML file with new_text. Empty new_text deletes. Line numbers must reflect the current file — re-read between multiple edits.",
		},
		func(tctx agent.ToolContext, args replaceLinesArgs) (editFileResult, error) {
			em.start(args.Path)
			pathErr := validateHTMLPath(args.Path)
			if pathErr != nil {
				em.fail(args.Path, pathErr)
				return editFileResult{Error: pathErr.Error()}, nil
			}
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			obj, err := s.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.replace_lines", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			if obj.Content == "" {
				em.fail(args.Path, errors.New("file not found"))
				return editFileResult{Error: "file not found: " + args.Path}, nil
			}
			updated, err := spliceLines(obj.Content, args.StartLine, args.EndLine, args.NewText)
			if err != nil {
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			if len(updated) > maxHTMLFileBytes {
				sizeErr := fmt.Errorf("content too large after replace_lines: %d bytes (max %d)", len(updated), maxHTMLFileBytes)
				em.fail(args.Path, sizeErr)
				return editFileResult{Error: sizeErr.Error()}, nil
			}
			guardErr := state.guard.Allow(toolSignature("replace_lines", args.Path,
				fmt.Sprintf("%d-%d", args.StartLine, args.EndLine), args.NewText))
			if guardErr != nil {
				em.fail(args.Path, guardErr)
				return editFileResult{Error: guardErr.Error()}, nil
			}
			contentType := obj.ContentType
			if contentType == "" {
				contentType = "text/html; charset=utf-8"
			}
			err = s.Write(tctx, slug, args.Path, updated, contentType, obj.Metadata)
			if err != nil {
				slog.Warn("agent.replace_lines", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.replace_lines", "slug", slug, "path", args.Path,
				"start_line", args.StartLine, "end_line", args.EndLine, "new_len", len(args.NewText))
			em.done(args.Path)
			return editFileResult{OK: true, Replacements: args.EndLine - args.StartLine + 1, Hints: budgetHint(-1, state)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create replace_lines tool: %w", err)
	}
	return t, nil
}

// newInsertAtLineTool returns a tool that inserts content after a given line
// without replacing anything. after_line=0 prepends; after_line=total_lines
// appends. Mirrors the well-trodden semantics of Anthropic's text_editor
// `insert` command so models that know that interface get it for free.
func newInsertAtLineTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "insert_at_line"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "insert_at_line",
			Description: "Insert content after line N in an HTML file. after_line=0 prepends, after_line=total_lines appends. Inserted verbatim — include a trailing newline if needed.",
		},
		func(tctx agent.ToolContext, args insertAtLineArgs) (editFileResult, error) {
			em.start(args.Path)
			pathErr := validateHTMLPath(args.Path)
			if pathErr != nil {
				em.fail(args.Path, pathErr)
				return editFileResult{Error: pathErr.Error()}, nil
			}
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			obj, err := s.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.insert_at_line", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			if obj.Content == "" {
				em.fail(args.Path, errors.New("file not found"))
				return editFileResult{Error: "file not found: " + args.Path}, nil
			}
			updated, err := insertAfterLine(obj.Content, args.AfterLine, args.Content)
			if err != nil {
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			if len(updated) > maxHTMLFileBytes {
				sizeErr := fmt.Errorf("content too large after insert_at_line: %d bytes (max %d)", len(updated), maxHTMLFileBytes)
				em.fail(args.Path, sizeErr)
				return editFileResult{Error: sizeErr.Error()}, nil
			}
			guardErr := state.guard.Allow(toolSignature("insert_at_line", args.Path,
				strconv.Itoa(args.AfterLine), args.Content))
			if guardErr != nil {
				em.fail(args.Path, guardErr)
				return editFileResult{Error: guardErr.Error()}, nil
			}
			contentType := obj.ContentType
			if contentType == "" {
				contentType = "text/html; charset=utf-8"
			}
			err = s.Write(tctx, slug, args.Path, updated, contentType, obj.Metadata)
			if err != nil {
				slog.Warn("agent.insert_at_line", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return editFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.insert_at_line", "slug", slug, "path", args.Path,
				"after_line", args.AfterLine, "inserted_len", len(args.Content))
			em.done(args.Path)
			return editFileResult{OK: true, Replacements: 1, Hints: budgetHint(-1, state)}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create insert_at_line tool: %w", err)
	}
	return t, nil
}

// spliceLines replaces lines start..end (1-indexed, inclusive) with newText.
// Delegates to internal/textedit, shared with the MCP editing surface.
func spliceLines(content string, start, end int, newText string) (string, error) {
	return textedit.SpliceLines(content, start, end, newText)
}

// insertAfterLine inserts insertContent after line `after` (0 prepends,
// total_lines appends). Delegates to internal/textedit.
func insertAfterLine(content string, after int, insertContent string) (string, error) {
	return textedit.InsertAfterLine(content, after, insertContent)
}

// applyEdit is the byte-exact find/replace (with whitespace-tolerant fallback)
// behind edit_file and edit_function. The implementation — including the
// tolerant match and the actionable not-found diagnostics — lives in
// internal/textedit so the MCP edit tools share identical semantics.
func applyEdit(content, oldText, newText string, replaceAll bool) (string, int, string, error) {
	return textedit.ApplyEdit(content, oldText, newText, replaceAll)
}

const (
	grepDefaultMax = 50
	grepHardCap    = 200
	grepSnippetMax = 200
)

func newGrepFilesTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "grep_files"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "grep_files",
			Description: "Literal (case-sensitive, no regex) substring search across HTML pages and function handlers. Returns paths, 1-indexed line numbers, and snippets.",
		},
		func(tctx agent.ToolContext, args grepFilesArgs) (grepFilesResult, error) {
			em.start("")
			if args.Pattern == "" {
				em.fail("", errors.New("pattern required"))
				return grepFilesResult{Error: "pattern is required"}, nil
			}
			maxRes := args.MaxResults
			if maxRes <= 0 {
				maxRes = grepDefaultMax
			}
			if maxRes > grepHardCap {
				maxRes = grepHardCap
			}
			files, err := s.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.grep_files", "slug", slug, "err", err)
				em.fail("", err)
				return grepFilesResult{Error: err.Error()}, nil
			}
			sort.Strings(files)
			out := make([]grepMatch, 0, maxRes)
			total := 0
			truncated := false
			for _, f := range files {
				if !grepEligible(f) {
					continue
				}
				obj, rerr := s.Read(tctx, slug, f)
				if rerr != nil || obj.Content == "" {
					continue
				}
				out, total, truncated = appendFileMatches(out, total, maxRes, truncated, f, obj.Content, args.Pattern)
			}
			slog.Info("agent.grep_files", "slug", slug, "pattern_len", len(args.Pattern),
				"total", total, "returned", len(out), "truncated", truncated)
			em.done("")
			return grepFilesResult{Matches: out, TotalMatches: total, Truncated: truncated}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create grep_files tool: %w", err)
	}
	return t, nil
}

// appendFileMatches scans a single file's content for the literal pattern and
// extends out with up to (maxRes - len(out)) new matches. Anything past the cap
// is counted in totalMatches and flips truncated to true. Extracting this
// keeps newGrepFilesTool's cognitive complexity in check.
func appendFileMatches(out []grepMatch, totalMatches, maxRes int, truncated bool, path, content, pattern string) ([]grepMatch, int, bool) {
	if !strings.Contains(content, pattern) {
		return out, totalMatches, truncated
	}
	for i, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, pattern) {
			continue
		}
		totalMatches++
		if len(out) < maxRes {
			out = append(out, grepMatch{
				Path: path, LineNumber: i + 1, Snippet: truncateSnippet(line),
			})
		} else {
			truncated = true
		}
	}
	return out, totalMatches, truncated
}

// grepEligible decides whether a stored path is worth grepping. Delegates to
// internal/textedit, shared with the MCP grep_files tool.
func grepEligible(path string) bool {
	return textedit.GrepEligible(path)
}

func truncateSnippet(line string) string {
	return textedit.TruncateSnippet(line, grepSnippetMax)
}

func newListFilesTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "list_files"}
	t, err := functiontool.New(
		functiontool.Config{Name: "list_files", Description: "List all HTML files created so far"},
		func(tctx agent.ToolContext, _ struct{}) (listFilesResult, error) {
			em.start("")
			files, err := s.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_files", "slug", slug, "err", err)
				em.fail("", err)
				return listFilesResult{Error: err.Error()}, nil
			}
			html := make([]string, 0, len(files))
			for _, f := range files {
				if strings.HasSuffix(f, ".html") {
					html = append(html, f)
				}
			}
			slog.Info("agent.list_files", "slug", slug, "count", len(html))
			em.done("")
			return listFilesResult{Files: html}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create list_files tool: %w", err)
	}
	return t, nil
}

func newListAssetsTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "list_assets"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "list_assets",
			Description: "List uploaded image assets with path, alt text, and description. Embed with <img src=\"assets/filename.ext\" alt=\"...\">; use the alt verbatim. Description tells you which image fits where.",
		},
		func(tctx agent.ToolContext, _ struct{}) (listAssetsResult, error) {
			em.start("")
			files, err := s.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_assets", "slug", slug, "err", err)
				em.fail("", err)
				return listAssetsResult{Error: err.Error()}, nil
			}
			assets := collectAssetEntries(tctx, s, slug, files)
			slog.Info("agent.list_assets", "slug", slug, "count", len(assets))
			em.done("")
			return listAssetsResult{Assets: assets}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create list_assets tool: %w", err)
	}
	return t, nil
}

// collectAssetEntries reads each asset's metadata via the store (cached, so
// repeat calls are cheap) and returns the path/alt/description rows the
// list_assets tool surfaces to the agent.
func collectAssetEntries(ctx context.Context, s *store.Store, slug string, files []string) []assetEntry {
	out := make([]assetEntry, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f, "assets/") {
			continue
		}
		entry := assetEntry{Path: f}
		obj, err := s.Read(ctx, slug, f)
		if err != nil {
			slog.Warn("agent.list_assets.read", "slug", slug, "path", f, "err", err)
			out = append(out, entry)
			continue
		}
		if obj != nil {
			entry.Alt = obj.Metadata["alt"]
			entry.Description = obj.Metadata["description"]
		}
		out = append(out, entry)
	}
	return out
}

// formatTemplateChecks renders a template's declarative Checks as a short
// upfront requirement list so the agent's first pass already targets the
// invariants the lint loop will later assert. Without this the model only
// learns about a missing <h1> or <form> through a retry round-trip — every
// avoided retry skips a fresh ~5–7K-token prefix resend.
func formatTemplateChecks(checks []templates.Check) string {
	if len(checks) == 0 {
		return ""
	}
	lines := []string{"Your output will be validated against these requirements (the lint loop asserts them after every build):"}
	for _, c := range checks {
		if c.File == "" || len(c.MustContain) == 0 {
			continue
		}
		needles := make([]string, 0, len(c.MustContain))
		for _, n := range c.MustContain {
			needles = append(needles, fmt.Sprintf("`%s`", n))
		}
		line := fmt.Sprintf("- %s must contain %s", c.File, strings.Join(needles, " and "))
		if c.Message != "" {
			line += " — " + c.Message
		}
		lines = append(lines, line)
	}
	if len(lines) == 1 {
		return ""
	}
	return strings.Join(lines, "\n")
}

// buildInstruction layers the per-template addendum on top of the base system
// prompt and adds a one-liner whenever the template ships skeleton files, so
// the agent knows to inspect the existing filesystem before writing.
//
// Order matters for prompt caching. Providers that cache automatically
// (OpenAI, DeepSeek, Gemini, Grok, Groq, Moonshot via OpenRouter) reuse
// whatever stable prefix the request opens with, so we lay the parts down
// stablest-first: base prompt (every build), functions addendum (shared by
// every functions-enabled template), per-template addendum (template-stable),
// skeleton notice (template-stable), examples notice (template-stable
// content, but the names list is template-determined), build context (the
// per-build meta block — date, slug, mode), then attachments notice (the
// only per-request variable). Any reordering of these blocks invalidates
// the cache for every build that follows.
func buildInstruction(tmpl *templates.SiteTemplate, attachments []Attachment, bctx BuildContext) string {
	parts := []string{systemPrompt}
	if tmpl != nil {
		if tmpl.EnablesFunctions {
			parts = append(parts, functionsPrompt)
		}
		if tmpl.PromptAddendum != "" {
			parts = append(parts, tmpl.PromptAddendum)
		}
		if checks := formatTemplateChecks(tmpl.Checks); checks != "" {
			parts = append(parts, checks)
		}
		if len(tmpl.Skeleton) > 0 {
			parts = append(parts, "A starter skeleton has already been written for this site and pre-loaded into your conversation history via seeded list_files / read_file (and list_functions / read_function for handlers). Extend or refine the existing files rather than starting from scratch.")
		}
		if len(tmpl.Examples) > 0 {
			names := make([]string, 0, len(tmpl.Examples))
			for n := range tmpl.Examples {
				names = append(names, n)
			}
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("Reference exemplars for this template were pre-loaded via read_example calls: %s. Use them as inspiration for layout, type hierarchy, and DaisyUI component composition — do not copy markup verbatim. The user's content comes from the prompt and any attachments, not from these examples.", strings.Join(names, ", ")))
		}
	}
	if block := formatBuildContext(bctx); block != "" {
		parts = append(parts, block)
	}
	if len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, a.Name)
		}
		parts = append(parts, fmt.Sprintf("The user attached the following reference files (markdown or HTML): %s. Their contents were pre-loaded into your conversation history via read_attachment calls. Treat them as authoritative source for page copy unless the user's prompt says otherwise.", strings.Join(names, ", ")))
	}
	return strings.Join(parts, "\n\n")
}

// formatBuildContext renders the per-build meta block. An entirely zero-value
// BuildContext returns "" so unit tests and any caller that has not migrated
// yet do not get a garbage block. A populated Now alone is enough to render
// the date line; Slug/SiteURL render together when both are set.
func formatBuildContext(bctx BuildContext) string {
	lines := []string{}
	if !bctx.Now.IsZero() {
		lines = append(lines, "- Today: "+bctx.Now.Format("Monday, 2006-01-02"))
	}
	if bctx.Slug != "" && bctx.SiteURL != "" {
		lines = append(lines, fmt.Sprintf("- Site: %s at %s", bctx.Slug, bctx.SiteURL))
	} else if bctx.Slug != "" {
		lines = append(lines, "- Site: "+bctx.Slug)
	}
	mode := "initial build (skeleton seeded — extend it)"
	if bctx.IsEdit {
		mode = "follow-up edit (extend or surgically modify existing files; prefer edit_file / replace_lines over rewriting whole pages)"
	}
	// Mode is meaningful only when there is enough other context to anchor
	// it — without slug or date the agent has nothing to attach it to.
	if len(lines) > 0 {
		lines = append(lines, "- Mode: "+mode)
	}
	if len(lines) == 0 {
		return ""
	}
	return "Build context:\n" + strings.Join(lines, "\n")
}

const (
	functionsDir = "functions/"
	jsExt        = ".js"
)

const (
	maxHTMLFileBytes   = 256 * 1024
	maxHTMLFiles       = 25
	maxAgentIterations = 60
	toolGuardRingLen   = 3

	maxQuestionsPerBuild = 3
	askQuestionTimeout   = 5 * time.Minute
)

// buildState bundles per-run state that the runner loop and tool closures
// both touch. iters is incremented from the ADK event loop in Run; tool
// closures read it via Load() to surface a "iterations remaining" hint to
// the model so it can self-pace.
//
// writeMu serializes every mutating tool against the store. ADK dispatches
// each function call in a separate goroutine (base_flow.handleFunctionCalls),
// and OpenAI/Anthropic both default to parallel tool calls — so two
// edit_file/replace_lines/write_file calls on the same path would otherwise
// race at S3, where there's no per-key locking and last-write-wins silently
// drops work. Reads stay lock-free so the model still gets fan-out wins on
// list_files / read_file / grep_files batches.
type buildState struct {
	guard          *toolGuard
	iters          atomic.Int64
	writeMu        sync.Mutex
	questionsAsked atomic.Int32
}

func newBuildState() *buildState {
	return &buildState{guard: &toolGuard{}}
}

// countFunctionCalls returns the number of function-call parts on an ADK
// event. ADK dispatches each call in its own goroutine
// (base_flow.handleFunctionCalls), so a return value > 1 means parallel tool
// execution actually happened on this turn — useful for confirming the model
// is batching and that buildState.writeMu is doing real work.
func countFunctionCalls(ev *session.Event) int {
	if ev == nil || ev.Content == nil {
		return 0
	}
	n := 0
	for _, part := range ev.Content.Parts {
		if part != nil && part.FunctionCall != nil {
			n++
		}
	}
	return n
}

// toolGuard rejects the same write/edit being issued twice in a short
// window. Catches the most common failure mode beyond ADK's iteration cap:
// the model loops on the same fix idea, burning iterations without making
// progress. The existing OldText == NewText no-op short-circuit in edit_file
// fires before the guard, so the loop doesn't waste a slot on a legitimate
// reasoning artefact.
type toolGuard struct {
	mu     sync.Mutex
	recent [toolGuardRingLen]string
	next   int
}

// Allow returns nil when signature is new; otherwise an error naming the
// tool the model just repeated. Signatures are opaque to Allow — the caller
// builds them with toolSignature so the encoding stays consistent.
func (g *toolGuard) Allow(signature string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, r := range g.recent {
		if r != "" && r == signature {
			toolName, _, _ := strings.Cut(signature, "|")
			return fmt.Errorf("identical %s call repeated — read the current file and pick a different change", toolName)
		}
	}
	g.recent[g.next] = signature
	g.next = (g.next + 1) % len(g.recent)
	return nil
}

// toolSignature builds a stable key for the anti-loop guard. The tool name
// lives in the leading segment so Allow can surface it in the error;
// remaining parts get sha256'd to keep memory bounded regardless of payload
// size.
func toolSignature(toolName string, parts ...string) string {
	h := sha256.New()
	for i, p := range parts {
		if i > 0 {
			h.Write([]byte{0})
		}
		h.Write([]byte(p))
	}
	return toolName + "|" + hex.EncodeToString(h.Sum(nil))
}

// budgetHint composes the "X of Y HTML files used; ~N iterations remaining"
// string that goes back to the model in tool results. htmlCount < 0 omits
// the file-count clause (callers that don't already have a List in hand
// can pass -1 to avoid an extra round-trip).
func budgetHint(htmlCount int, state *buildState) string {
	remaining := maxAgentIterations - int(state.iters.Load())
	if remaining < 0 {
		remaining = 0
	}
	if htmlCount < 0 {
		return fmt.Sprintf("~%d iterations remaining", remaining)
	}
	return fmt.Sprintf("%d of %d HTML files used; ~%d iterations remaining", htmlCount, maxHTMLFiles, remaining)
}

// validateHTMLPath gates every tool that writes/edits HTML: reject anything
// that could escape the slug, smuggle non-HTML into HTML paths, or clobber
// files managed by other tools. Implemented in internal/textedit so the MCP
// write/edit tools enforce the identical rule.
func validateHTMLPath(p string) error {
	return textedit.ValidateHTMLPath(p)
}

// validateFunctionName accepts the bare handler name (no path, no extension)
// supplied to write_function/read_function. Delegates to internal/textedit.
func validateFunctionName(name string) error {
	return textedit.ValidateFunctionName(name)
}

func newWriteFunctionTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "write_function"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "write_function",
			Description: "Write a server-side handler JS file to functions/{name}.js. Source must be a CommonJS module: module.exports = function(request) { ... }. See the 'Dynamic features' section for available globals.",
		},
		func(tctx agent.ToolContext, args writeFunctionArgs) (writeFunctionResult, error) {
			path := functionsDir + args.Name + jsExt
			err := validateFunctionName(args.Name)
			if err != nil {
				em.fail(path, err)
				return writeFunctionResult{Error: err.Error()}, nil
			}
			em.start(path)
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			err = s.Write(tctx, slug, path, args.Source, "application/javascript; charset=utf-8", nil)
			if err != nil {
				slog.Warn("agent.write_function", "slug", slug, "name", args.Name, "err", err)
				em.fail(path, err)
				return writeFunctionResult{Error: err.Error()}, nil
			}
			slog.Info("agent.write_function", "slug", slug, "name", args.Name, "length", len(args.Source))
			em.done(path)
			return writeFunctionResult{OK: true, Path: path}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create write_function tool: %w", err)
	}
	return t, nil
}

func newReadFunctionTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "read_function"}
	t, err := functiontool.New(
		functiontool.Config{Name: "read_function", Description: "Read the source of an existing functions/{name}.js handler."},
		func(tctx agent.ToolContext, args readFunctionArgs) (readFunctionResult, error) {
			path := functionsDir + args.Name + jsExt
			err := validateFunctionName(args.Name)
			if err != nil {
				em.fail(path, err)
				return readFunctionResult{Error: err.Error()}, nil
			}
			em.start(path)
			obj, err := s.Read(tctx, slug, path)
			if err != nil {
				slog.Warn("agent.read_function", "slug", slug, "name", args.Name, "err", err)
				em.fail(path, err)
				return readFunctionResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_function", "slug", slug, "name", args.Name, "length", len(obj.Content))
			em.done(path)
			return readFunctionResult{Source: obj.Content}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_function tool: %w", err)
	}
	return t, nil
}

func newEditFunctionTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "edit_function"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "edit_function",
			Description: "Surgical edit on a functions/<name>.js handler: same semantics as edit_file. Prefer this over rewriting the whole handler.",
		},
		func(tctx agent.ToolContext, args editFunctionArgs) (editFunctionResult, error) {
			path := functionsDir + args.Name + jsExt
			err := validateFunctionName(args.Name)
			if err != nil {
				em.fail(path, err)
				return editFunctionResult{Error: err.Error()}, nil
			}
			em.start(path)
			if args.OldText == "" {
				em.fail(path, errors.New("old_text required"))
				return editFunctionResult{Error: "old_text is required"}, nil
			}
			if args.OldText == args.NewText {
				em.done(path)
				return editFunctionResult{
					OK:   true,
					Path: path,
					Note: "old_text and new_text are identical; no change made",
				}, nil
			}
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			obj, err := s.Read(tctx, slug, path)
			if err != nil {
				slog.Warn("agent.edit_function", "slug", slug, "name", args.Name, "err", err)
				em.fail(path, err)
				return editFunctionResult{Error: err.Error()}, nil
			}
			if obj.Content == "" {
				em.fail(path, errors.New("function not found"))
				return editFunctionResult{Error: "function not found: " + args.Name}, nil
			}
			updated, count, note, applyErr := applyEdit(obj.Content, args.OldText, args.NewText, args.ReplaceAll)
			if applyErr != nil {
				em.fail(path, applyErr)
				return editFunctionResult{Error: applyErr.Error()}, nil
			}
			err = s.Write(tctx, slug, path, updated, "application/javascript; charset=utf-8", nil)
			if err != nil {
				slog.Warn("agent.edit_function", "slug", slug, "name", args.Name, "err", err)
				em.fail(path, err)
				return editFunctionResult{Error: err.Error()}, nil
			}
			slog.Info("agent.edit_function", "slug", slug, "name", args.Name,
				"old_len", len(args.OldText), "new_len", len(args.NewText), "replacements", count)
			em.done(path)
			return editFunctionResult{OK: true, Path: path, Replacements: count, Note: note}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create edit_function tool: %w", err)
	}
	return t, nil
}

func newDeleteFunctionTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "delete_function"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "delete_function",
			Description: "Remove a functions/<name>.js handler. The /api/<name> endpoint will return 404 after deletion. HTML pages cannot be deleted — leave stale pages in place or rewrite with write_file.",
		},
		func(tctx agent.ToolContext, args deleteFunctionArgs) (deleteFunctionResult, error) {
			path := functionsDir + args.Name + jsExt
			err := validateFunctionName(args.Name)
			if err != nil {
				em.fail(path, err)
				return deleteFunctionResult{Error: err.Error()}, nil
			}
			em.start(path)
			state.writeMu.Lock()
			defer state.writeMu.Unlock()
			err = s.Delete(tctx, slug, path)
			if err != nil {
				slog.Warn("agent.delete_function", "slug", slug, "name", args.Name, "err", err)
				em.fail(path, err)
				return deleteFunctionResult{Error: err.Error()}, nil
			}
			slog.Info("agent.delete_function", "slug", slug, "name", args.Name)
			em.done(path)
			return deleteFunctionResult{OK: true, Path: path}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create delete_function tool: %w", err)
	}
	return t, nil
}

func newListFunctionsTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "list_functions"}
	t, err := functiontool.New(
		functiontool.Config{Name: "list_functions", Description: "List handler names currently under functions/. Each name maps to /api/{name}."},
		func(tctx agent.ToolContext, _ struct{}) (listFunctionsResult, error) {
			em.start("")
			files, err := s.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_functions", "slug", slug, "err", err)
				em.fail("", err)
				return listFunctionsResult{Error: err.Error()}, nil
			}
			names := make([]string, 0, len(files))
			for _, f := range files {
				if !strings.HasPrefix(f, functionsDir) || !strings.HasSuffix(f, jsExt) {
					continue
				}
				bare := strings.TrimSuffix(strings.TrimPrefix(f, functionsDir), jsExt)
				if bare != "" {
					names = append(names, bare)
				}
			}
			slog.Info("agent.list_functions", "slug", slug, "count", len(names))
			em.done("")
			return listFunctionsResult{Functions: names}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create list_functions tool: %w", err)
	}
	return t, nil
}

// invokeAskUser contains the core ask_user logic, extracted so tests can call
// it directly without going through the ADK tool framework.
func invokeAskUser(ctx context.Context, args askUserArgs, slug string, tracker *events.Tracker, emit func(events.Event), state *buildState, timeout time.Duration) (askUserResult, error) {
	// Per-build cap: short-circuit immediately when exceeded.
	n := state.questionsAsked.Add(1)
	if n > maxQuestionsPerBuild {
		slog.Info("agent.ask_user.cap_reached", "slug", slug, "n", n)
		return askUserResult{Answer: args.Recommendation, Source: "limit_reached"}, nil
	}

	// Validation — surface errors as data so the agent can self-correct.
	switch {
	case strings.TrimSpace(args.Question) == "":
		return askUserResult{Error: "question is required"}, nil
	case strings.TrimSpace(args.Recommendation) == "":
		return askUserResult{Error: "recommendation is required"}, nil
	case strings.TrimSpace(args.Why) == "":
		return askUserResult{Error: "why is required"}, nil
	case len(args.Options) > 4:
		return askUserResult{Error: "options must have at most 4 entries"}, nil
	}

	// Truncate to keep SSE payloads bounded.
	q := truncate(args.Question, 200)
	rec := truncate(args.Recommendation, 200)
	why := truncate(args.Why, 400)
	opts := make([]string, len(args.Options))
	for i, o := range args.Options {
		opts[i] = truncate(o, 80)
	}

	// Generate a short random question ID.
	var raw [8]byte
	_, randErr := rand.Read(raw[:])
	if randErr != nil {
		return askUserResult{Error: "failed to generate question id: " + randErr.Error()}, nil
	}
	qid := hex.EncodeToString(raw[:])

	ev := events.Event{
		Type:           events.TypeQuestion,
		Phase:          events.PhaseAsk,
		QuestionID:     qid,
		Question:       q,
		Recommendation: rec,
		Why:            why,
		Options:        opts,
	}

	emit(events.Event{Type: events.TypeTool, Tool: "ask_user", Phase: events.PhaseStart})

	ch := tracker.Ask(slug, ev)

	select {
	case answer, ok := <-ch:
		if !ok {
			// Channel closed by terminal-status cleanup — return recommendation.
			emit(events.Event{Type: events.TypeTool, Tool: "ask_user", Phase: events.PhaseDone})
			return askUserResult{Answer: rec, Source: "recommendation_timeout"}, nil
		}
		slog.Info("agent.ask_user.answered", "slug", slug, "qid", qid)
		emit(events.Event{Type: events.TypeTool, Tool: "ask_user", Phase: events.PhaseDone})
		return askUserResult{Answer: answer, Source: "user"}, nil
	case <-time.After(timeout):
		// Emit timeout event so workspace removes the question card.
		tracker.EmitTimeout(slug, qid, rec)
		slog.Info("agent.ask_user.timeout", "slug", slug, "qid", qid)
		emit(events.Event{Type: events.TypeTool, Tool: "ask_user", Phase: events.PhaseDone})
		return askUserResult{Answer: rec, Source: "recommendation_timeout"}, nil
	case <-ctx.Done():
		emit(events.Event{Type: events.TypeTool, Tool: "ask_user", Phase: events.PhaseError, Message: "build cancelled"})
		return askUserResult{}, fmt.Errorf("ask_user cancelled: %w", ctx.Err())
	}
}

// newAskUserTool creates the ask_user tool that lets the agent pause and ask a
// plain-language question. The agent goroutine blocks on a channel until the
// workspace POSTs an answer, the timeout fires, or the build context is
// cancelled. On timeout the recommendation is returned so the build
// self-completes even if the user wandered off.
func newAskUserTool(slug string, tracker *events.Tracker, emit func(events.Event), state *buildState) (tool.Tool, error) {
	t, err := functiontool.New(
		functiontool.Config{
			Name: "ask_user",
			Description: "Pause the build and ask the user a plain-language content or design question. " +
				"Only use when the prompt is silent on something that materially changes what you build. " +
				"Never ask about technical details (components, file names, themes). " +
				"At most 3 questions per build — make a reasonable choice instead whenever possible.",
		},
		func(tctx agent.ToolContext, args askUserArgs) (askUserResult, error) {
			return invokeAskUser(tctx, args, slug, tracker, emit, state, askQuestionTimeout)
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create ask_user tool: %w", err)
	}
	return t, nil
}

// truncate shortens s to at most n runes (not bytes).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
