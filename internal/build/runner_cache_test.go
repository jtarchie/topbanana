package build

import (
	"context"
	"errors"
	"testing"

	"github.com/jtarchie/buildabear/internal/agent"
	"github.com/jtarchie/buildabear/internal/events"
	"github.com/jtarchie/buildabear/internal/store"
	"github.com/jtarchie/buildabear/internal/templates"
)

// fakeRunner is a Runner that records its identity but does no work. The
// methods panic if called because the cache tests below never invoke
// them — they only assert which Runner runnerFor returns.
type fakeRunner struct{ id string }

func (f *fakeRunner) Run(_ context.Context, _ *store.Store, _, _ string, _ *templates.SiteTemplate, _ []agent.Attachment, _ []agent.SeedToolCall, _ func(events.Event)) error {
	panic("fakeRunner.Run should not be called in cache tests")
}

func (f *fakeRunner) Describe(_ context.Context, _ *store.Store, _, _ string) (agent.SiteDescription, error) {
	panic("fakeRunner.Describe should not be called in cache tests")
}

// TestRunnerCache pins the per-user-model cache contract:
//
//   - Empty Params.Model returns the default runner unchanged.
//   - Params.Model equal to the configured Model also returns the default.
//   - A novel model triggers RunnerFactory exactly once, then subsequent
//     calls return the same Runner without re-invoking the factory.
//   - Factory errors propagate wrapped.
//   - With nil RunnerFactory, non-default models silently fall back to
//     the default runner (the path tests rely on).
//
// per subtest; extracting helpers would obscure the contract being pinned.
//
//nolint:gocognit,cyclop // table-driven test with explicit assertions
func TestRunnerCache(t *testing.T) {
	t.Run("empty model returns default runner", func(t *testing.T) {
		dflt := &fakeRunner{id: "default"}
		svc := &Service{
			runner:  dflt,
			model:   "openai/gpt-4",
			runners: map[string]Runner{},
		}
		got, err := svc.runnerFor(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != Runner(dflt) {
			t.Fatalf("expected default runner, got %#v", got)
		}
	})

	t.Run("model matching default returns default runner", func(t *testing.T) {
		dflt := &fakeRunner{id: "default"}
		svc := &Service{
			runner:  dflt,
			model:   "openai/gpt-4",
			runners: map[string]Runner{},
		}
		got, err := svc.runnerFor(context.Background(), "openai/gpt-4")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != Runner(dflt) {
			t.Fatalf("expected default runner on Model==default, got %#v", got)
		}
	})

	t.Run("novel model invokes factory once and caches", func(t *testing.T) {
		dflt := &fakeRunner{id: "default"}
		want := &fakeRunner{id: "claude"}
		calls := 0
		factory := func(_ context.Context, model string) (Runner, error) {
			calls++
			if model != "anthropic/claude-3" {
				t.Fatalf("factory called with %q, want anthropic/claude-3", model)
			}
			return want, nil
		}
		svc := &Service{
			runner:        dflt,
			model:         "openai/gpt-4",
			runnerFactory: factory,
			runners:       map[string]Runner{},
		}

		first, err := svc.runnerFor(context.Background(), "anthropic/claude-3")
		if err != nil {
			t.Fatalf("first call err: %v", err)
		}
		if first != Runner(want) {
			t.Fatalf("first call: got %#v want %#v", first, want)
		}
		if calls != 1 {
			t.Fatalf("factory should fire exactly once on first miss, got %d calls", calls)
		}

		second, err := svc.runnerFor(context.Background(), "anthropic/claude-3")
		if err != nil {
			t.Fatalf("second call err: %v", err)
		}
		if second != first {
			t.Fatalf("cache miss on repeat: got %#v want %#v", second, first)
		}
		if calls != 1 {
			t.Fatalf("factory re-fired on cache hit, got %d calls", calls)
		}
	})

	t.Run("factory error surfaces as wrapped error", func(t *testing.T) {
		want := errors.New("upstream blew up")
		svc := &Service{
			runner:        &fakeRunner{id: "default"},
			model:         "openai/gpt-4",
			runnerFactory: func(context.Context, string) (Runner, error) { return nil, want },
			runners:       map[string]Runner{},
		}
		_, err := svc.runnerFor(context.Background(), "anthropic/claude-3")
		if err == nil {
			t.Fatalf("expected factory error to propagate")
		}
		if !errors.Is(err, want) {
			t.Fatalf("expected wrap of %v, got %v", want, err)
		}
	})

	t.Run("nil factory falls back to default runner", func(t *testing.T) {
		dflt := &fakeRunner{id: "default"}
		svc := &Service{
			runner:  dflt,
			model:   "openai/gpt-4",
			runners: map[string]Runner{},
		}
		got, err := svc.runnerFor(context.Background(), "anthropic/claude-3")
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
		if got != Runner(dflt) {
			t.Fatalf("nil factory should fall back to default, got %#v", got)
		}
	})
}
