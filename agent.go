package main

import (
	"context"
	"fmt"
	"log/slog"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/agent/llmagent"
	adkmodel "google.golang.org/adk/model"
	"google.golang.org/adk/runner"
	"google.golang.org/adk/session"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"
)

type writeFileArgs struct {
	Path    string `json:"path"`    // relative path for the HTML file (e.g. index.html)
	Content string `json:"content"` // full HTML content to write
}

type writeFileResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type readFileArgs struct {
	Path string `json:"path"` // relative path to read (e.g. index.html)
}

type readFileResult struct {
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}

type listFilesResult struct {
	Files []string `json:"files"`
	Error string   `json:"error,omitempty"`
}

func runAgent(ctx context.Context, llm adkmodel.LLM, store *Store, slug, prompt string) error {
	writeTool, err := functiontool.New(
		functiontool.Config{Name: "write_file", Description: "Write content to an HTML file"},
		func(ctx tool.Context, args writeFileArgs) (writeFileResult, error) { //nolint:contextcheck
			slog.Info("agent.write_file", "slug", slug, "path", args.Path, "length", len(args.Content))
			err := store.Write(ctx, slug, args.Path, args.Content)
			if err != nil {
				slog.Debug("agent.write_file.error", "slug", slug, "path", args.Path, "err", err)
				return writeFileResult{Error: err.Error()}, nil
			}

			slog.Info("agent.write_file.ok", "slug", slug, "path", args.Path)
			return writeFileResult{OK: true}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create write_file tool: %w", err)
	}

	readTool, err := functiontool.New(
		functiontool.Config{Name: "read_file", Description: "Read content from an HTML file"},
		func(ctx tool.Context, args readFileArgs) (readFileResult, error) { //nolint:contextcheck
			slog.Info("agent.read_file", "slug", slug, "path", args.Path)
			obj, err := store.Read(ctx, slug, args.Path)
			if err != nil {
				slog.Debug("agent.read_file.error", "slug", slug, "path", args.Path, "err", err)
				return readFileResult{Error: err.Error()}, nil
			}
			slog.Info("agent.read_file.ok", "slug", slug, "path", args.Path, "length", len(obj.Content))
			return readFileResult{Content: obj.Content}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create read_file tool: %w", err)
	}

	listTool, err := functiontool.New(
		functiontool.Config{Name: "list_files", Description: "List all HTML files created so far"},
		func(ctx tool.Context, _ struct{}) (listFilesResult, error) { //nolint:contextcheck
			slog.Info("agent.list_files", "slug", slug)
			files, err := store.List(ctx, slug)
			if err != nil {
				slog.Debug("agent.list_files.error", "slug", slug, "err", err)
				return listFilesResult{Error: err.Error()}, nil
			}
			slog.Info("agent.list_files.ok", "slug", slug, "count", len(files))
			return listFilesResult{Files: files}, nil
		},
	)
	if err != nil {
		return fmt.Errorf("create list_files tool: %w", err)
	}

	a, err := llmagent.New(llmagent.Config{
		Name:        "html-builder",
		Description: "Builds static HTML apps from a prompt",
		Instruction: systemPrompt,
		Model:       llm,
		Tools:       []tool.Tool{writeTool, readTool, listTool},
	})
	if err != nil {
		return fmt.Errorf("create agent: %w", err)
	}

	sessionSvc := session.InMemoryService()

	r, err := runner.New(runner.Config{
		AppName:           "buildabear",
		Agent:             a,
		SessionService:    sessionSvc,
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
