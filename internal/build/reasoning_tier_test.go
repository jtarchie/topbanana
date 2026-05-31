package build

import (
	"context"
	"testing"

	"google.golang.org/genai"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/bloomhollow/internal/model"
)

func TestReasoningForTier_OnlyAuthorReasons(t *testing.T) {
	t.Parallel()

	svc := &Service{reasoningEffort: genai.ThinkingLevel("high")}

	if got := svc.reasoningForTier(model.TierAuthor); got != "high" {
		t.Errorf("Author reasoning = %q, want high", got)
	}
	for _, tier := range []model.Tier{model.TierEditor, model.TierUtility, model.TierVision} {
		if got := svc.reasoningForTier(tier); got != "" {
			t.Errorf("%s reasoning = %q, want off", tier, got)
		}
	}
}

// runnerForTier caches runners by (model ID, reasoning effort). Author and
// Editor on the same model now resolve to different reasoning levels, so they
// must get distinct runner wrappers — but the expensive LLM factory must still
// fire only once for the shared model.
func TestRunnerForTier_SharedModelDistinctReasoningOneFactoryCall(t *testing.T) {
	t.Parallel()

	calls := 0
	svc := newSvc()
	svc.reasoningEffort = genai.ThinkingLevel("high")
	svc.tierMap = model.TierMap{
		model.TierAuthor: "shared-model",
		model.TierEditor: "shared-model",
	}
	svc.llmFactory = func(_ context.Context, id string) (adkmodel.LLM, error) {
		calls++
		return &stubLLM{id: id}, nil
	}

	authorRunner, _, err := svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err != nil {
		t.Fatalf("Author: %v", err)
	}
	editorRunner, _, err := svc.runnerForTier(context.Background(), nil, model.TierEditor)
	if err != nil {
		t.Fatalf("Editor: %v", err)
	}

	if calls != 1 {
		t.Errorf("factory fired %d times for one shared model, want 1", calls)
	}

	ar, ok := authorRunner.(agentRunner)
	if !ok {
		t.Fatalf("author runner is %T, want agentRunner", authorRunner)
	}
	er, ok := editorRunner.(agentRunner)
	if !ok {
		t.Fatalf("editor runner is %T, want agentRunner", editorRunner)
	}
	if ar.reasoningEffort != "high" {
		t.Errorf("author reasoning = %q, want high", ar.reasoningEffort)
	}
	if er.reasoningEffort != "" {
		t.Errorf("editor reasoning = %q, want off", er.reasoningEffort)
	}
	if ar.llm != er.llm {
		t.Error("author and editor should share the one cached LLM client")
	}
}
