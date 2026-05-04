package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

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

func runAgent(ctx context.Context, llm adkmodel.LLM, store *Store, slug, prompt string, tmpl *SiteTemplate) error {
	// tool.Context chains down to context.Context via interface embedding, so it can be
	// passed to store methods directly. The contextcheck linter wants the outer ctx
	// propagated into the closures, but tool callbacks fire later from the runner with
	// their own per-invocation context — that is the correct one to forward.
	writeTool, err := functiontool.New(
		functiontool.Config{Name: "write_file", Description: "Write content to an HTML file"},
		func(tctx tool.Context, args writeFileArgs) (writeFileResult, error) { //nolint:contextcheck
			err := store.Write(tctx, slug, args.Path, args.Content)
			if err != nil {
				slog.Warn("agent.write_file", "slug", slug, "path", args.Path, "err", err)
				return writeFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.write_file", "slug", slug, "path", args.Path, "length", len(args.Content))
			return writeFileResult{OK: true}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create write_file tool: %w", err)
	}

	readTool, err := functiontool.New(
		functiontool.Config{Name: "read_file", Description: "Read content from an HTML file"},
		func(tctx tool.Context, args readFileArgs) (readFileResult, error) { //nolint:contextcheck
			obj, err := store.Read(tctx, slug, args.Path)
			if err != nil {
				slog.Warn("agent.read_file", "slug", slug, "path", args.Path, "err", err)
				return readFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_file", "slug", slug, "path", args.Path, "length", len(obj.Content))
			return readFileResult{Content: obj.Content}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create read_file tool: %w", err)
	}

	listTool, err := functiontool.New(
		functiontool.Config{Name: "list_files", Description: "List all HTML files created so far"},
		func(tctx tool.Context, _ struct{}) (listFilesResult, error) { //nolint:contextcheck
			files, err := store.List(tctx, slug)
			if err != nil {
				slog.Warn("agent.list_files", "slug", slug, "err", err)
				return listFilesResult{Error: err.Error()}, nil
			}
			slog.Info("agent.list_files", "slug", slug, "count", len(files))
			return listFilesResult{Files: files}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create list_files tool: %w", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "html-builder",
		Description: "Builds static HTML apps from a prompt",
		Instruction: buildInstruction(tmpl),
		Model:       llm,
		Tools:       []tool.Tool{writeTool, readTool, listTool},
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	r, err := runner.New(runner.Config{
		AppName:           "buildabear",
		Agent:             a,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		return fmt.Errorf("create runner: %w", err)
	}

	userMsg := &genai.Content{
		Parts: []*genai.Part{genai.NewPartFromText(prompt)},
		Role:  "user",
	}

	for event, err := range r.Run(ctx, slug, slug, userMsg, agent.RunConfig{}) {
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
