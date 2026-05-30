// Package agent wires the ADK runner, tools, and system prompt for a single
// build. It also provides the vision-captioning entrypoint used during asset
// uploads, since both consume the configured LLM model.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "embed"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/jtarchie/bloomhollow/internal/events"
	"github.com/jtarchie/bloomhollow/internal/store"
	"github.com/jtarchie/bloomhollow/internal/templates"
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

// Run invokes the agent against the given slug. emit may be nil. attachments
// are inlined into the session as seeded read_attachment(name) call/response
// pairs ahead of the caller-supplied seeds. reasoningEffort, when non-empty,
// asks the model to spend reasoning tokens before each response — supported
// by Gemini 2.5/3 Flash + Pro, Claude Sonnet/Opus/Haiku, Qwen Plus, GPT-5,
// DeepSeek V3.1+ and other reasoning-capable models on OpenRouter.
func Run(ctx context.Context, llm adkmodel.LLM, s *store.Store, slug, prompt string, tmpl *templates.SiteTemplate, attachments []Attachment, seeds []SeedToolCall, reasoningEffort genai.ThinkingLevel, emit func(events.Event)) error {
	if emit == nil {
		emit = func(events.Event) {}
	}

	state := newBuildState()

	// contextcheck flags this because Run has a ctx, but the tools fire later
	// under per-invocation contexts from the runner; passing ctx would be wrong.
	tools, err := buildAgentTools(s, slug, tmpl, attachments, emit, state) //nolint:contextcheck
	if err != nil {
		return err
	}

	cfg := llmagent.Config{
		Name:        "html-builder",
		Description: "Builds static HTML apps from a prompt",
		Instruction: buildInstruction(tmpl, attachments),
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
		return fmt.Errorf("create agent: %w", err)
	}

	// Examples first (aspirational references the model should see before
	// anything else), then attachments (the user's authoritative content),
	// then caller-supplied seeds (per-page reads, list_files, etc).
	allSeeds := append(ExampleSeeds(tmpl), AttachmentSeeds(attachments)...)
	allSeeds = append(allSeeds, seeds...)

	sessSvc := session.InMemoryService()
	sess, err := seedSession(ctx, sessSvc, slug, allSeeds)
	if err != nil {
		return err
	}

	r, err := runner.New(runner.Config{
		AppName:           "bloomhollow",
		Agent:             a,
		SessionService:    sessSvc,
		AutoCreateSession: false,
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}

	userMsg := &genai.Content{
		Parts: []*genai.Part{genai.NewPartFromText(prompt)},
		Role:  "user",
	}

	for event, err := range r.Run(ctx, sess.UserID(), sess.ID(), userMsg, agent.RunConfig{}) {
		if err != nil {
			return fmt.Errorf("agent error: %w", err)
		}
		iters := state.iters.Add(1)
		if iters > maxAgentIterations {
			slog.Warn("agent.iteration_cap", "slug", slug, "iterations", iters, "max", maxAgentIterations)
			return fmt.Errorf("agent exceeded %d iterations without finalizing", maxAgentIterations)
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

	return nil
}

// seedSession creates a fresh session for the given slug and pre-populates it
// with synthetic tool-call/response pairs.
func seedSession(ctx context.Context, sessSvc session.Service, slug string, seeds []SeedToolCall) (session.Session, error) {
	createResp, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "bloomhollow",
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
// tool.Context chains down to context.Context via interface embedding, so it's
// passed to store methods directly. Tool callbacks fire later from the runner
// with their own per-invocation context — that is the correct one to forward
// (contextcheck objects to this but is wrong).
//
// The function-authoring tools (write_function, read_function, list_functions)
// are only registered when the template opts in via EnablesFunctions. Older
// brochure templates see no behavioural change.
func buildAgentTools(s *store.Store, slug string, tmpl *templates.SiteTemplate, attachments []Attachment, emit func(events.Event), state *buildState) ([]tool.Tool, error) {
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
	return tools, nil
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
			Description: "Read a markdown or HTML file the user attached to this build/edit submission. Each returned line is prefixed with its 1-indexed line number and a tab, same convention as read_file. Use these attachments as authoritative source for page copy. The full set of names was pre-loaded into your conversation history; call this tool only if you need to re-read.",
		},
		func(_ tool.Context, args readAttachmentArgs) (readAttachmentResult, error) {
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
			Description: "Read an aspirational reference HTML page for this template. Examples are not the user's content — they are hand-crafted demonstrations of layout, type hierarchy, DaisyUI component usage, and visual rhythm. Use them as inspiration, never copy markup verbatim. The full set was pre-loaded into your conversation history via seeded read_example calls; call this tool only if you need to re-read.",
		},
		func(_ tool.Context, args readExampleArgs) (readExampleResult, error) {
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
func ExampleSeeds(tmpl *templates.SiteTemplate) []SeedToolCall {
	if tmpl == nil || len(tmpl.Examples) == 0 {
		return nil
	}
	names := make([]string, 0, len(tmpl.Examples))
	for n := range tmpl.Examples {
		names = append(names, n)
	}
	sort.Strings(names)
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
		func(tctx tool.Context, args writeFileArgs) (writeFileResult, error) {
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
			Description: "Read content from an HTML file. Each returned line is prefixed with its 1-indexed line number and a tab (`   42\\t<section>`), so the number you pass to replace_lines/insert_at_line is the number you see — no counting newlines. Optionally pass start_line and end_line (1-indexed, inclusive) to read only a slice; line numbers in the slice still refer to the original file. total_lines is always returned. The leading `<number>\\t` is annotation, not file content — strip it before passing text back to write_file/edit_file/replace_lines/insert_at_line.",
		},
		func(tctx tool.Context, args readFileArgs) (readFileResult, error) {
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

// NumberLines prefixes every line in content with its 1-indexed line number
// (right-aligned to 6 columns, followed by a tab), matching the cat -n
// convention LLMs recognize from training. startOffset is the number to
// assign to the first line — pass 1 for a full-file read or the requested
// start_line for a slice, so numbers always refer to positions in the
// original file rather than the slice's local index. The empty string
// returns the empty string. Exported so internal/build can apply the same
// transformation to seeded read_file responses.
func NumberLines(content string, startOffset int) string {
	if content == "" {
		return ""
	}
	if startOffset < 1 {
		startOffset = 1
	}
	lines := strings.Split(content, "\n")
	var out strings.Builder
	out.Grow(len(content) + len(lines)*8)
	for i, line := range lines {
		if i > 0 {
			out.WriteByte('\n')
		}
		fmt.Fprintf(&out, "%6d\t%s", startOffset+i, line)
	}
	return out.String()
}

// sliceLines returns a 1-indexed-inclusive slice of content delimited by \n,
// plus the total line count of the full content. start/end of 0 mean "from
// line 1" and "through last line" respectively, so the zero-value (both 0)
// returns the whole content unchanged. start past the end returns an empty
// slice with no error (lets the agent self-correct using total_lines).
func sliceLines(content string, start, end int) (string, int, error) {
	var total int
	if content == "" {
		total = 0
	} else {
		total = strings.Count(content, "\n") + 1
	}
	if start <= 0 && end <= 0 {
		return content, total, nil
	}
	if start > 0 && end > 0 && start > end {
		return "", total, errors.New("start_line must be <= end_line")
	}
	lines := strings.Split(content, "\n")
	if start <= 0 {
		start = 1
	}
	if end <= 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return "", total, nil
	}
	return strings.Join(lines[start-1:end], "\n"), total, nil
}

func newEditFileTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "edit_file"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "edit_file",
			Description: "Replace exact text in an existing HTML file. Provide old_text (must match verbatim — include enough surrounding context to be unique) and new_text. Prefer this over write_file for surgical changes: rewriting whole files wastes tokens and risks regressions in unrelated content. Set replace_all=true to replace every occurrence.",
		},
		func(tctx tool.Context, args editFileArgs) (editFileResult, error) {
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
			Description: "Replace lines start_line..end_line (1-indexed, inclusive) in an HTML file with new_text. Use when read_file showed you the exact line range and you want to avoid whitespace-matching headaches. Pass empty new_text to delete those lines. Both line numbers refer to the file as it exists right now; if you make multiple replacements, re-read between them to get fresh numbers.",
		},
		func(tctx tool.Context, args replaceLinesArgs) (editFileResult, error) {
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
			Description: "Insert content after line N (1-indexed) in an HTML file. Use after_line=0 to prepend, after_line=total_lines to append. Content is inserted verbatim — include a trailing newline if you want a clean break before the next line.",
		},
		func(tctx tool.Context, args insertAtLineArgs) (editFileResult, error) {
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

// spliceLines replaces lines start..end (1-indexed, inclusive) of content
// with newText. Validates the range; returns a descriptive error if the
// range is out-of-bounds or inverted so the agent can correct itself.
func spliceLines(content string, start, end int, newText string) (string, error) {
	if start < 1 {
		return "", fmt.Errorf("start_line must be >= 1 (got %d)", start)
	}
	if end < start {
		return "", fmt.Errorf("end_line (%d) must be >= start_line (%d)", end, start)
	}
	lines := strings.Split(content, "\n")
	if end > len(lines) {
		return "", fmt.Errorf("end_line %d exceeds file length %d", end, len(lines))
	}
	// strings.Split on newText so multi-line replacements rejoin cleanly with
	// the surrounding context. Empty newText splices in nothing (i.e. deletes
	// the range).
	var head, mid, tail []string
	head = lines[:start-1]
	tail = lines[end:]
	if newText != "" {
		mid = strings.Split(newText, "\n")
	}
	out := make([]string, 0, len(head)+len(mid)+len(tail))
	out = append(out, head...)
	out = append(out, mid...)
	out = append(out, tail...)
	return strings.Join(out, "\n"), nil
}

// insertAfterLine returns content with insertContent spliced in after line
// `after` (1-indexed). after=0 prepends; after=total_lines appends.
func insertAfterLine(content string, after int, insertContent string) (string, error) {
	lines := strings.Split(content, "\n")
	if after < 0 {
		return "", fmt.Errorf("after_line must be >= 0 (got %d)", after)
	}
	if after > len(lines) {
		return "", fmt.Errorf("after_line %d exceeds file length %d", after, len(lines))
	}
	insertLines := strings.Split(insertContent, "\n")
	out := make([]string, 0, len(lines)+len(insertLines))
	out = append(out, lines[:after]...)
	out = append(out, insertLines...)
	out = append(out, lines[after:]...)
	return strings.Join(out, "\n"), nil
}

// applyEdit performs the find/replace at the heart of edit_file and
// edit_function. Returns the updated content, the replacement count, a note
// surfaced to the caller (empty when there's nothing to flag), and an error.
// Returning errors as values (not Go errors) lets the caller surface them in
// the tool's Error field so the agent can recover.
//
// When exact-string matching fails, applyEdit attempts a whitespace-tolerant
// search: if the file contains exactly one byte range whose whitespace-
// collapsed form equals the whitespace-collapsed old_text, that range is
// replaced and a note advises the model to copy whitespace verbatim next
// time. Zero or multiple tolerant matches still fall through to the original
// diagnostic so the model has actionable feedback.
func applyEdit(content, oldText, newText string, replaceAll bool) (string, int, string, error) {
	count := strings.Count(content, oldText)
	if count == 0 {
		updated, ok := applyTolerantEdit(content, oldText, newText)
		if ok {
			return updated, 1, "applied a whitespace-tolerant match — the file's whitespace at the match site differed from old_text. Re-read the file (use read_file with start_line/end_line) to copy whitespace verbatim for predictable edits next time.", nil
		}
		return "", 0, "", diagnoseNotFound(content, oldText)
	}
	if count > 1 && !replaceAll {
		return "", 0, "", fmt.Errorf("old_text matches %d locations; include more surrounding context to make it unique, or set replace_all=true", count)
	}
	if replaceAll {
		return strings.ReplaceAll(content, oldText, newText), count, "", nil
	}
	return strings.Replace(content, oldText, newText, 1), 1, "", nil
}

// applyTolerantEdit looks for exactly one substring of content whose
// whitespace-collapsed form equals collapseWS(oldText). When that uniquely
// identifies a region, it's safe to replace — the alternative is forcing the
// agent to re-read and retry, which is what wasted retries in the failing
// build. When zero or >1 candidates exist, returns ok=false so the caller
// falls through to the existing error path.
func applyTolerantEdit(content, oldText, newText string) (string, bool) {
	target := collapseWS(oldText)
	if target == "" {
		return "", false
	}
	type span struct{ start, end int }
	var found []span
	for i := 0; i <= len(content); i++ {
		// Find the smallest j > i such that collapseWS(content[i:j]) == target.
		// Once equal we record it and resume the outer loop past the match;
		// once it exceeds target length we abandon this start.
		for j := i; j <= len(content); j++ {
			collapsed := collapseWS(content[i:j])
			if collapsed == target {
				found = append(found, span{i, j})
				if len(found) > 1 {
					return "", false // ambiguous — bail
				}
				i = j - 1 // -1 because outer loop's i++ will bump it
				break
			}
			if len(collapsed) > len(target) {
				break
			}
		}
	}
	if len(found) != 1 {
		return "", false
	}
	m := found[0]
	return content[:m.start] + newText + content[m.end:], true
}

// diagnoseNotFound returns the most actionable error message for a failed
// edit_file lookup. The common failure mode is that the model copied old_text
// with slightly-wrong whitespace (tabs vs spaces, missing indent, extra
// trailing newline), so we check for those first and tell the model exactly
// what to fix. Falling back to a generic message would just trigger another
// blind retry.
func diagnoseNotFound(content, oldText string) error {
	trimmed := strings.TrimSpace(oldText)
	if trimmed != "" && trimmed != oldText && strings.Contains(content, trimmed) {
		return errors.New("old_text has extra leading or trailing whitespace that the file does not contain; trim it and retry")
	}
	if containsCollapsedWS(content, oldText) {
		return errors.New("old_text matches only when whitespace is normalized — the file uses different indentation, tabs, or line breaks than your old_text. Re-read the file (use start_line/end_line to zoom in) and copy the whitespace verbatim")
	}
	return errors.New("old_text not found in file. Re-read the file to confirm the exact text (whitespace included), or use grep_files to locate a unique substring before retrying")
}

// containsCollapsedWS reports whether needle appears in haystack after every
// run of whitespace is collapsed to a single space in both. Used only for
// diagnostics — the actual replace still requires a byte-exact match.
func containsCollapsedWS(haystack, needle string) bool {
	return strings.Contains(collapseWS(haystack), collapseWS(needle))
}

func collapseWS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inWS := false
	for _, r := range s {
		switch r {
		case ' ', '\t', '\n', '\r':
			if !inWS {
				b.WriteByte(' ')
				inWS = true
			}
		default:
			b.WriteRune(r)
			inWS = false
		}
	}
	return b.String()
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
			Description: "Search a literal (case-sensitive, no regex) substring across all HTML pages and function handlers. Returns matching paths with 1-indexed line numbers and snippets. Use before edit_file to find the unique surrounding context you need.",
		},
		func(tctx tool.Context, args grepFilesArgs) (grepFilesResult, error) {
			em.start("")
			if args.Pattern == "" {
				em.fail("", errors.New("pattern required"))
				return grepFilesResult{Error: "pattern is required"}, nil
			}
			max := args.MaxResults
			if max <= 0 {
				max = grepDefaultMax
			}
			if max > grepHardCap {
				max = grepHardCap
			}
			files, err := s.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.grep_files", "slug", slug, "err", err)
				em.fail("", err)
				return grepFilesResult{Error: err.Error()}, nil
			}
			sort.Strings(files)
			out := make([]grepMatch, 0, max)
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
				out, total, truncated = appendFileMatches(out, total, max, truncated, f, obj.Content, args.Pattern)
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
// extends out with up to (max - len(out)) new matches. Anything past the cap
// is counted in totalMatches and flips truncated to true. Extracting this
// keeps newGrepFilesTool's cognitive complexity in check.
func appendFileMatches(out []grepMatch, totalMatches, max int, truncated bool, path, content, pattern string) ([]grepMatch, int, bool) {
	if !strings.Contains(content, pattern) {
		return out, totalMatches, truncated
	}
	for i, line := range strings.Split(content, "\n") {
		if !strings.Contains(line, pattern) {
			continue
		}
		totalMatches++
		if len(out) < max {
			out = append(out, grepMatch{
				Path: path, LineNumber: i + 1, Snippet: truncateSnippet(line),
			})
		} else {
			truncated = true
		}
	}
	return out, totalMatches, truncated
}

// grepEligible decides whether a stored path is worth grepping. Assets are
// binary-ish, the sidecar is internal metadata, and other extensions don't
// belong in a search aimed at HTML + function source.
func grepEligible(path string) bool {
	if strings.HasPrefix(path, "assets/") {
		return false
	}
	if path == ".bloomhollow.json" || path == ".buildabear.json" {
		return false
	}
	return strings.HasSuffix(path, ".html") || strings.HasSuffix(path, ".js")
}

func truncateSnippet(line string) string {
	if len(line) <= grepSnippetMax {
		return line
	}
	return line[:grepSnippetMax] + "…"
}

func newListFilesTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "list_files"}
	t, err := functiontool.New(
		functiontool.Config{Name: "list_files", Description: "List all HTML files created so far"},
		func(tctx tool.Context, _ struct{}) (listFilesResult, error) {
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
			Description: "List uploaded image assets with their alt text and descriptions. Embed an asset with <img src=\"assets/filename.ext\" alt=\"...\"> using the alt verbatim. Use the description to decide which images to use and where.",
		},
		func(tctx tool.Context, _ struct{}) (listAssetsResult, error) {
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
// content, but the names list is template-determined), then attachments
// notice (the only per-request variable). Any reordering of these blocks
// invalidates the cache for every build that follows.
func buildInstruction(tmpl *templates.SiteTemplate, attachments []Attachment) string {
	parts := []string{systemPrompt}
	if tmpl != nil {
		if tmpl.EnablesFunctions {
			parts = append(parts, functionsPrompt)
		}
		if tmpl.PromptAddendum != "" {
			parts = append(parts, tmpl.PromptAddendum)
		}
		if len(tmpl.Skeleton) > 0 {
			parts = append(parts, "A starter skeleton has already been written for this site. Call list_files and read_file before deciding what to write — extend or refine the existing files rather than starting from scratch.")
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
	if len(attachments) > 0 {
		names := make([]string, 0, len(attachments))
		for _, a := range attachments {
			names = append(names, a.Name)
		}
		parts = append(parts, fmt.Sprintf("The user attached the following reference files (markdown or HTML): %s. Their contents were pre-loaded into your conversation history via read_attachment calls. Treat them as authoritative source for page copy unless the user's prompt says otherwise.", strings.Join(names, ", ")))
	}
	return strings.Join(parts, "\n\n")
}

const (
	functionsDir = "functions/"
	jsExt        = ".js"
)

const (
	maxHTMLFileBytes   = 256 * 1024
	maxHTMLFiles       = 25
	maxHTMLPathLen     = 200
	maxAgentIterations = 60
	toolGuardRingLen   = 3
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
	guard   *toolGuard
	iters   atomic.Int64
	writeMu sync.Mutex
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

// reservedWritePrefixes are paths managed by other tools (functions/, assets/)
// that the HTML write tools must not clobber.
var reservedWritePrefixes = []string{"functions/", "assets/"}

// reservedWritePaths are exact paths the HTML write tools must not touch
// (e.g. the per-site sidecar persisted by the build service). Both the
// current and legacy sidecar names are reserved so an agent on a legacy
// site can't accidentally clobber pre-rebrand metadata.
var reservedWritePaths = map[string]bool{
	".bloomhollow.json": true,
	".buildabear.json":  true,
}

// validateHTMLPath gates every tool that writes/edits HTML. Mirrors
// validateFunctionName's posture: reject anything that could escape the slug,
// smuggle non-HTML into HTML paths, or clobber files managed by other tools.
func validateHTMLPath(p string) error {
	for _, check := range htmlPathChecks {
		err := check(p)
		if err != nil {
			return err
		}
	}
	return nil
}

var htmlPathChecks = []func(string) error{
	checkHTMLPathShape,
	checkHTMLPathCharset,
	checkHTMLPathSegments,
	checkHTMLPathExtension,
	checkHTMLPathReserved,
}

func checkHTMLPathShape(p string) error {
	switch {
	case p == "":
		return errors.New("path is required")
	case len(p) > maxHTMLPathLen:
		return fmt.Errorf("path too long (max %d chars)", maxHTMLPathLen)
	case strings.HasPrefix(p, "/"):
		return errors.New("path must be relative (no leading /)")
	case strings.Contains(p, `\`):
		return errors.New("path must use forward slashes")
	}
	return nil
}

func checkHTMLPathCharset(p string) error {
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_' || r == '/' || r == '.':
		default:
			return fmt.Errorf("path must match [a-z0-9_/.-] (got %q)", p)
		}
	}
	return nil
}

func checkHTMLPathSegments(p string) error {
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q contains an empty or relative segment", p)
		}
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not canonical", p)
	}
	return nil
}

func checkHTMLPathExtension(p string) error {
	if !strings.HasSuffix(p, ".html") {
		return fmt.Errorf("path %q must end with .html", p)
	}
	return nil
}

func checkHTMLPathReserved(p string) error {
	if reservedWritePaths[p] {
		return fmt.Errorf("path %q is reserved", p)
	}
	for _, pfx := range reservedWritePrefixes {
		if strings.HasPrefix(p, pfx) {
			return fmt.Errorf("path %q is under reserved prefix %q", p, pfx)
		}
	}
	return nil
}

// validateFunctionName accepts the bare handler name (no path, no extension)
// the agent supplies to write_function/read_function. We reject anything that
// could escape the slug's functions/ prefix or smuggle JS into a non-function
// path. Names match [a-z0-9-_]{1,40}.
func validateFunctionName(name string) error {
	if name == "" {
		return errors.New("function name is required")
	}
	if len(name) > 40 {
		return errors.New("function name too long")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-' || r == '_':
		default:
			return errors.New("function name must match [a-z0-9-_]")
		}
	}
	return nil
}

func newWriteFunctionTool(s *store.Store, slug string, emit func(events.Event), state *buildState) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "write_function"}
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "write_function",
			Description: "Write a server-side handler JS file to functions/{name}.js. Source must be a CommonJS module: module.exports = function(request) { ... }. See the 'Dynamic features' section for available globals.",
		},
		func(tctx tool.Context, args writeFunctionArgs) (writeFunctionResult, error) {
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
		func(tctx tool.Context, args readFunctionArgs) (readFunctionResult, error) {
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
			Description: "Replace exact text in an existing functions/<name>.js handler. Same semantics as edit_file but for JS handlers. Prefer this over write_function for surgical changes.",
		},
		func(tctx tool.Context, args editFunctionArgs) (editFunctionResult, error) {
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
		func(tctx tool.Context, args deleteFunctionArgs) (deleteFunctionResult, error) {
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
		func(tctx tool.Context, _ struct{}) (listFunctionsResult, error) {
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
