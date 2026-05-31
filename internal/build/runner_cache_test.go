package build

import (
	"context"
	"errors"
	"iter"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// fakeRunner is a Runner that records its identity but does no work. The
// methods panic if called because the cache tests below never invoke
// them — they only assert which Runner runnerForTier returns.
type fakeRunner struct{ id string }

func (f *fakeRunner) Run(_ context.Context, _ *store.Store, _, _ string, _ *templates.SiteTemplate, _ []agent.Attachment, _ []agent.SeedToolCall, _ time.Time, _ bool, _ func(events.Event)) (agent.Usage, error) {
	panic("fakeRunner.Run should not be called in cache tests")
}

func (f *fakeRunner) Describe(_ context.Context, _ *store.Store, _, _ string) (agent.SiteDescription, error) {
	panic("fakeRunner.Describe should not be called in cache tests")
}

// stubLLM is an adkmodel.LLM that exists only to be stored in the cache
// and identified by the name() method. Iterating its GenerateContent
// panics — runnerForTier never invokes it, only wraps it in an
// agentRunner.
type stubLLM struct{ id string }

func (s *stubLLM) Name() string { return s.id }
func (s *stubLLM) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	panic("stubLLM.GenerateContent should not be called in cache tests")
}

func newSvc() *Service {
	return &Service{
		runners: map[string]Runner{},
		llms:    map[string]adkmodel.LLM{},
	}
}

func TestRunnerForTier_TestInjectionPerTier(t *testing.T) {
	t.Parallel()

	author := &fakeRunner{id: "author"}
	editor := &fakeRunner{id: "editor"}

	svc := newSvc()
	svc.runnersByTier = map[model.Tier]Runner{
		model.TierAuthor: author,
		model.TierEditor: editor,
	}

	got, _, err := svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err != nil || got != Runner(author) {
		t.Fatalf("Author tier: got %#v err=%v, want author", got, err)
	}
	got, _, err = svc.runnerForTier(context.Background(), nil, model.TierEditor)
	if err != nil || got != Runner(editor) {
		t.Fatalf("Editor tier: got %#v err=%v, want editor", got, err)
	}
}

func TestRunnerForTier_LegacyRunnerServesAllTiers(t *testing.T) {
	t.Parallel()

	r := &fakeRunner{id: "legacy"}
	svc := newSvc()
	svc.runner = r

	for _, tier := range model.AllTiers {
		got, _, err := svc.runnerForTier(context.Background(), nil, tier)
		if err != nil || got != Runner(r) {
			t.Errorf("tier %q: got %#v err=%v, want legacy runner", tier, got, err)
		}
	}
}

func TestRunnerForTier_FactoryInvokedOnceCachedByModelID(t *testing.T) {
	t.Parallel()

	calls := 0
	llm := &stubLLM{id: "shared-model"}
	svc := newSvc()
	svc.tierMap = model.TierMap{
		model.TierAuthor:  "shared-model",
		model.TierEditor:  "shared-model", // same model for two tiers
		model.TierUtility: "other-model",
	}
	svc.llmFactory = func(_ context.Context, id string) (adkmodel.LLM, error) {
		calls++
		if id == "shared-model" {
			return llm, nil
		}
		return &stubLLM{id: id}, nil
	}

	_, idA, err := svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err != nil || idA != "shared-model" {
		t.Fatalf("Author: id=%q err=%v", idA, err)
	}
	_, idE, err := svc.runnerForTier(context.Background(), nil, model.TierEditor)
	if err != nil || idE != "shared-model" {
		t.Fatalf("Editor: id=%q err=%v", idE, err)
	}
	if calls != 1 {
		t.Errorf("factory called %d times for two tiers on same model, want 1", calls)
	}

	// Distinct model invokes factory again.
	_, idU, err := svc.runnerForTier(context.Background(), nil, model.TierUtility)
	if err != nil || idU != "other-model" {
		t.Fatalf("Utility: id=%q err=%v", idU, err)
	}
	if calls != 2 {
		t.Errorf("factory called %d times after distinct tier, want 2", calls)
	}

	// Repeat Author call hits cache.
	_, _, _ = svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if calls != 2 {
		t.Errorf("factory re-fired on cache hit, total calls = %d", calls)
	}
}

func TestRunnerForTier_PerBuildOverrideMergesOnTopOfDefaults(t *testing.T) {
	t.Parallel()

	svc := newSvc()
	svc.tierMap = model.TierMap{model.TierAuthor: "default-author"}
	svc.llmFactory = func(_ context.Context, id string) (adkmodel.LLM, error) {
		return &stubLLM{id: id}, nil
	}

	override := model.TierMap{model.TierEditor: "user-editor"}
	_, got, err := svc.runnerForTier(context.Background(), override, model.TierEditor)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "user-editor" {
		t.Errorf("Editor with override = %q, want user-editor", got)
	}

	// Tier the override didn't touch falls back to the service default.
	_, got, err = svc.runnerForTier(context.Background(), override, model.TierAuthor)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "default-author" {
		t.Errorf("Author with override (no entry) = %q, want default-author", got)
	}
}

func TestRunnerForTier_FactoryErrorSurfacesAsWrappedError(t *testing.T) {
	t.Parallel()

	want := errors.New("upstream blew up")
	svc := newSvc()
	svc.tierMap = model.TierMap{model.TierAuthor: "x"}
	svc.llmFactory = func(context.Context, string) (adkmodel.LLM, error) { return nil, want }

	_, _, err := svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err == nil {
		t.Fatalf("expected error")
	}
	if !errors.Is(err, want) {
		t.Errorf("expected wrap of %v, got %v", want, err)
	}
}

func TestRunnerForTier_NoFactoryReturnsError(t *testing.T) {
	t.Parallel()

	svc := newSvc()
	svc.tierMap = model.TierMap{model.TierAuthor: "x"}
	// no llmFactory, no runner, no runnersByTier

	_, _, err := svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err == nil {
		t.Errorf("expected error when no factory configured")
	}
}

func TestLLMForTier_SharesCacheWithRunnerForTier(t *testing.T) {
	t.Parallel()

	calls := 0
	llm := &stubLLM{id: "vision-model"}
	svc := newSvc()
	svc.tierMap = model.TierMap{
		model.TierAuthor: "vision-model",
		model.TierVision: "vision-model",
	}
	svc.llmFactory = func(_ context.Context, id string) (adkmodel.LLM, error) {
		calls++
		return llm, nil
	}

	// Vision tier through LLMForTier populates the cache.
	gotLLM, _, err := svc.LLMForTier(context.Background(), nil, model.TierVision)
	if err != nil || gotLLM != adkmodel.LLM(llm) {
		t.Fatalf("LLMForTier: got %#v err=%v", gotLLM, err)
	}
	if calls != 1 {
		t.Errorf("calls after LLMForTier = %d, want 1", calls)
	}

	// Author resolves to the same model ID — should hit the cache.
	_, _, err = svc.runnerForTier(context.Background(), nil, model.TierAuthor)
	if err != nil {
		t.Fatalf("runnerForTier(Author): %v", err)
	}
	if calls != 1 {
		t.Errorf("factory re-fired on cache hit: calls = %d", calls)
	}
}
