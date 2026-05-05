package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

// seedToolCall is a synthetic tool-call/response pair that the caller can
// pre-populate in the agent's session. The model sees these as if it had
// already issued the call and received the response, so we can skip
// round-trips for things we already know (the file list, the content of
// pages the user named in the prompt).
type seedToolCall struct {
	Name     string
	Args     map[string]any
	Response map[string]any
}

// Tool results surface errors as data (Error field) rather than as a Go error: this lets
// the model see the failure in the tool response and recover (e.g. retry with a different
// path) instead of aborting the run.

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

func runAgent(ctx context.Context, llm adkmodel.LLM, store *Store, slug, prompt string, tmpl *SiteTemplate, seeds []seedToolCall, emit func(BuildEvent)) error {
	if emit == nil {
		emit = func(BuildEvent) {}
	}

	// contextcheck flags this because runAgent has a ctx, but the tools fire
	// later under per-invocation contexts from the runner; passing ctx would
	// be wrong, not right.
	tools, err := buildAgentTools(store, slug, emit)
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
// with synthetic tool-call/response pairs. The model sees this on its first
// turn as if it had already made those calls — saves a round-trip for things
// the caller already knows (file list, content of named pages).
func seedSession(ctx context.Context, sessSvc session.Service, slug string, seeds []seedToolCall) (session.Session, error) {
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

// buildAgentTools constructs the four tools the agent uses against a single
// site. Pulled out of runAgent so the latter stays under the cognitive
// complexity ceiling. tool.Context chains down to context.Context via interface
// embedding, so it's passed to store methods directly. The contextcheck linter
// wants the outer ctx propagated into the closures, but tool callbacks fire
// later from the runner with their own per-invocation context — that is the
// correct one to forward.
func buildAgentTools(store *Store, slug string, emit func(BuildEvent)) ([]tool.Tool, error) {
	builders := []func(*Store, string, func(BuildEvent)) (tool.Tool, error){
		newWriteFileTool,
		newReadFileTool,
		newListFilesTool,
		newListAssetsTool,
	}
	tools := make([]tool.Tool, 0, len(builders))
	for _, b := range builders {
		t, err := b(store, slug, emit)
		if err != nil {
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

func newWriteFileTool(store *Store, slug string, emit func(BuildEvent)) (tool.Tool, error) {
	t, err := functiontool.New(
		functiontool.Config{Name: "write_file", Description: "Write content to an HTML file"},
		func(tctx tool.Context, args writeFileArgs) (writeFileResult, error) {
			emit(BuildEvent{Type: "tool", Tool: "write_file", Phase: "start", Path: args.Path})
			err := store.Write(tctx, slug, args.Path, args.Content, "text/html; charset=utf-8", nil)
			if err != nil {
				slog.Warn("agent.write_file", "slug", slug, "path", args.Path, "err", err)
				emit(BuildEvent{Type: "tool", Tool: "write_file", Phase: "error", Path: args.Path, Message: err.Error()})
				return writeFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.write_file", "slug", slug, "path", args.Path, "length", len(args.Content))
			emit(BuildEvent{Type: "tool", Tool: "write_file", Phase: "done", Path: args.Path})
			return writeFileResult{OK: true}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create write_file tool: %w", err)
	}
	return t, nil
}

func newReadFileTool(store *Store, slug string, emit func(BuildEvent)) (tool.Tool, error) {
	t, err := functiontool.New(
		functiontool.Config{Name: "read_file", Description: "Read content from an HTML file"},
		func(tctx tool.Context, args readFileArgs) (readFileResult, error) {
			emit(BuildEvent{Type: "tool", Tool: "read_file", Phase: "start", Path: args.Path})
			obj, err := store.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.read_file", "slug", slug, "path", args.Path, "err", err)
				emit(BuildEvent{Type: "tool", Tool: "read_file", Phase: "error", Path: args.Path, Message: err.Error()})
				return readFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_file", "slug", slug, "path", args.Path, "length", len(obj.Content))
			emit(BuildEvent{Type: "tool", Tool: "read_file", Phase: "done", Path: args.Path})
			return readFileResult{Content: obj.Content}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create read_file tool: %w", err)
	}
	return t, nil
}

func newListFilesTool(store *Store, slug string, emit func(BuildEvent)) (tool.Tool, error) {
	t, err := functiontool.New(
		functiontool.Config{Name: "list_files", Description: "List all HTML files created so far"},
		func(tctx tool.Context, _ struct{}) (listFilesResult, error) {
			emit(BuildEvent{Type: "tool", Tool: "list_files", Phase: "start"})
			files, err := store.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_files", "slug", slug, "err", err)
				emit(BuildEvent{Type: "tool", Tool: "list_files", Phase: "error", Message: err.Error()})
				return listFilesResult{Error: err.Error()}, nil
			}
			html := make([]string, 0, len(files))
			for _, f := range files {
				if strings.HasSuffix(f, ".html") {
					html = append(html, f)
				}
			}
			slog.Info("agent.list_files", "slug", slug, "count", len(html))
			emit(BuildEvent{Type: "tool", Tool: "list_files", Phase: "done"})
			return listFilesResult{Files: html}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create list_files tool: %w", err)
	}
	return t, nil
}

func newListAssetsTool(store *Store, slug string, emit func(BuildEvent)) (tool.Tool, error) {
	t, err := functiontool.New(
		functiontool.Config{
			Name:        "list_assets",
			Description: "List uploaded image assets with their alt text and descriptions. Embed an asset with <img src=\"assets/filename.ext\" alt=\"...\"> using the alt verbatim. Use the description to decide which images to use and where.",
		},
		func(tctx tool.Context, _ struct{}) (listAssetsResult, error) {
			emit(BuildEvent{Type: "tool", Tool: "list_assets", Phase: "start"})
			files, err := store.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_assets", "slug", slug, "err", err)
				emit(BuildEvent{Type: "tool", Tool: "list_assets", Phase: "error", Message: err.Error()})
				return listAssetsResult{Error: err.Error()}, nil
			}
			assets := collectAssetEntries(tctx, store, slug, files)
			slog.Info("agent.list_assets", "slug", slug, "count", len(assets))
			emit(BuildEvent{Type: "tool", Tool: "list_assets", Phase: "done"})
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
func collectAssetEntries(ctx context.Context, store *Store, slug string, files []string) []assetEntry {
	out := make([]assetEntry, 0, len(files))
	for _, f := range files {
		if !strings.HasPrefix(f, "assets/") {
			continue
		}
		entry := assetEntry{Path: f}
		obj, err := store.Read(ctx, slug, f)
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
func buildInstruction(tmpl *SiteTemplate) string {
	if tmpl == nil {
		return systemPrompt
	}
	parts := []string{systemPrompt}
	if tmpl.PromptAddendum != "" {
		parts = append(parts, tmpl.PromptAddendum)
	}
	if len(tmpl.Skeleton) > 0 {
		parts = append(parts, "A starter skeleton has already been written for this site. Call list_files and read_file before deciding what to write — extend or refine the existing files rather than starting from scratch.")
	}
	return strings.Join(parts, "\n\n")
}
