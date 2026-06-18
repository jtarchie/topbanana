package agent

import (
	"errors"
	"fmt"
	"log/slog"

	"google.golang.org/adk/agent"
	"google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"

	"github.com/jtarchie/topbanana/internal/docs"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
)

type searchDocsArgs struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

type searchDocsResult struct {
	Results []docs.Result `json:"results"`
	Error   string        `json:"error,omitempty"`
}

// newSearchDocsTool exposes the embedded daisyUI reference (internal/docs) as an
// on-demand keyword search. It is read-only and idempotent — no store, no slug,
// no anti-loop guard — so its builder matches the plainBuilders signature and
// drops into that slice. The corpus is NOT seeded into the prompt, so a build
// pays for it only when the agent actually calls it; that is the point of a pull
// tool over preloading the whole reference.
func newSearchDocsTool(_ *store.Store, _ string, emit func(events.Event)) (tool.Tool, error) {
	em := emitter{emit: emit, tool: "search_docs"}
	t, err := functiontool.New(
		functiontool.Config{
			Name: "search_docs",
			Description: "Keyword-search the vendored daisyUI component reference (class names, modifiers, parts, usage rules). " +
				"Use ONLY when you are unsure which daisyUI class or modifier to use, or how a component composes — " +
				"e.g. \"badge sizes\", \"btn-primary\", \"timeline horizontal\", \"card actions\", \"theme colors\". " +
				"Returns the matching reference sections. Do NOT call it for components you already know " +
				"(btn, card, navbar, hero) or for plain HTML/CSS. This replaces any need to search the web.",
		},
		func(_ agent.ToolContext, args searchDocsArgs) (searchDocsResult, error) {
			em.start("")
			if args.Query == "" {
				err := errors.New("query is required")
				em.fail("", err)
				return searchDocsResult{Error: err.Error()}, nil
			}
			results := docs.Search(args.Query, docs.Options{MaxResults: args.MaxResults})
			slog.Info("agent.search_docs", "query_len", len(args.Query), "results", len(results))
			em.done("")
			return searchDocsResult{Results: results}, nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("create search_docs tool: %w", err)
	}
	return t, nil
}
