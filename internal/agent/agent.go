// Package agent wires the ADK runner, tools, and system prompt for a single
// build. It also provides the vision-captioning entrypoint used during asset
// uploads, since both consume the configured LLM model.
package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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

	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
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
}

type readFileArgs struct {
	Path string `json:"path"`
}

type readFileResult struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
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

// Run invokes the agent against the given slug. emit may be nil.
func Run(ctx context.Context, llm adkmodel.LLM, s *store.Store, slug, prompt string, tmpl *templates.SiteTemplate, seeds []SeedToolCall, emit func(events.Event)) error {
	if emit == nil {
		emit = func(events.Event) {}
	}

	// contextcheck flags this because Run has a ctx, but the tools fire later
	// under per-invocation contexts from the runner; passing ctx would be wrong.
	tools, err := buildAgentTools(s, slug, tmpl, emit)
	if err != nil {
		return err
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "html-builder",
		Description: "Builds static HTML apps from a prompt",
		Instruction: buildInstruction(tmpl),
		Model:       llm,
		Tools:       tools,
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	sessSvc := session.InMemoryService()
	sess, err := seedSession(ctx, sessSvc, slug, seeds)
	if err != nil {
		return err
	}

	r, err := runner.New(runner.Config{
		AppName:           "buildabear",
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
		if event != nil && event.IsFinalResponse() {
			slog.Info("agent.done", "slug", slug)
			break
		}
	}

	return nil
}

// seedSession creates a fresh session for the given slug and pre-populates it
// with synthetic tool-call/response pairs.
func seedSession(ctx context.Context, sessSvc session.Service, slug string, seeds []SeedToolCall) (session.Session, error) {
	createResp, err := sessSvc.Create(ctx, &session.CreateRequest{
		AppName:   "buildabear",
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
func buildAgentTools(s *store.Store, slug string, tmpl *templates.SiteTemplate, emit func(events.Event)) ([]tool.Tool, error) {
	builders := []func(*store.Store, string, func(events.Event)) (tool.Tool, error){
		newWriteFileTool,
		newReadFileTool,
		newListFilesTool,
		newListAssetsTool,
	}
	if tmpl != nil && tmpl.EnablesFunctions {
		builders = append(builders,
			newWriteFunctionTool,
			newReadFunctionTool,
			newListFunctionsTool,
		)
	}
	tools := make([]tool.Tool, 0, len(builders))
	for _, b := range builders {
		t, err := b(s, slug, emit)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func newWriteFileTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "write_file"}
	t, err := functiontool.New(
		functiontool.Config{Name: "write_file", Description: "Write content to an HTML file"},
		func(tctx tool.Context, args writeFileArgs) (writeFileResult, error) {
			em.start(args.Path)
			err := s.Write(tctx, slug, args.Path, args.Content, "text/html; charset=utf-8", nil)
			if err != nil {
				slog.Warn("agent.write_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return writeFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.write_file", "slug", slug, "path", args.Path, "length", len(args.Content))
			em.done(args.Path)
			return writeFileResult{OK: true}, nil
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
		functiontool.Config{Name: "read_file", Description: "Read content from an HTML file"},
		func(tctx tool.Context, args readFileArgs) (readFileResult, error) {
			em.start(args.Path)
			obj, err := s.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.read_file", "slug", slug, "path", args.Path, "err", err)
				em.fail(args.Path, err)
				return readFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_file", "slug", slug, "path", args.Path, "length", len(obj.Content))
			em.done(args.Path)
			return readFileResult{Content: obj.Content}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_file tool: %w", err)
	}
	return t, nil
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
func buildInstruction(tmpl *templates.SiteTemplate) string {
	if tmpl == nil {
		return systemPrompt
	}
	parts := []string{systemPrompt}
	if tmpl.PromptAddendum != "" {
		parts = append(parts, tmpl.PromptAddendum)
	}
	if tmpl.EnablesFunctions {
		parts = append(parts, functionsPrompt)
	}
	if len(tmpl.Skeleton) > 0 {
		parts = append(parts, "A starter skeleton has already been written for this site. Call list_files and read_file before deciding what to write — extend or refine the existing files rather than starting from scratch.")
	}
	return strings.Join(parts, "\n\n")
}

const (
	functionsDir = "functions/"
	jsExt        = ".js"
)

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

func newWriteFunctionTool(s *store.Store, slug string, emit func(events.Event)) (tool.Tool, error) {
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
