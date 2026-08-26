package build

// The result event is the workspace's verdict: which files a run changed and
// what the agent said on the way out. These tests lock in the three cases that
// matter — a real change, the incident-shaped "completed but changed nothing"
// run, and machinery log keys that must stay silent.

import (
	"context"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// writingRunner writes index.html and emits tool events with the bare path
// the production agent uses ("index.html", no leading slash) — the recorder
// reads the file back through the store, so the path must resolve.
type writingRunner struct{}

func (writingRunner) Run(ctx context.Context, s *store.Store, req RunRequest, emit func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "index.html"})
	err := s.Write(ctx, req.Slug, "index.html", validIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		return agent.Usage{}, err //nolint:wrapcheck // test stub passthrough
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseDone, Path: "index.html"})
	emit(events.Event{Type: events.TypeAgentText, Message: "done"})
	return agent.Usage{}, nil
}

func (writingRunner) Describe(context.Context, *store.Store, string, string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

// resultEvents filters a slug's event history down to the result verdicts.
func resultEvents(t *testing.T, tracker *events.Tracker, slug string) []events.Event {
	t.Helper()
	out := []events.Event{}
	for _, ev := range collectHistory(t, tracker, slug) {
		if ev.Type == events.TypeResult {
			out = append(out, ev)
		}
	}
	return out
}

func newResultTestService(t *testing.T, runner Runner) (*Service, *events.Tracker) {
	t.Helper()
	st := minioStoreForBuild(t)
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		RecordEdit:   true,
		BuildTimeout: 30 * time.Second,
	})
	return svc, tracker
}

func TestService_Start_EmitsResultWithChangedFiles(t *testing.T) {
	t.Parallel()

	svc, tracker := newResultTestService(t, writingRunner{})
	slug := buildSlug(t)
	cleanupSlug(t, svc.store, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "build it",
		LogKey:       "build",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})
	if status := waitForTerminal(t, tracker, slug, 30*time.Second); status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}

	results := resultEvents(t, tracker, slug)
	if len(results) != 1 {
		t.Fatalf("result events = %d, want exactly 1", len(results))
	}
	found := false
	for _, f := range results[0].Files {
		if f == "index.html" {
			found = true
		}
	}
	if !found {
		t.Fatalf("result files = %v, want index.html present", results[0].Files)
	}
	if results[0].Message != "done" {
		t.Fatalf("result message = %q, want the agent's closing text", results[0].Message)
	}
}

func TestService_Start_EmitsEmptyResultWhenNothingChanged(t *testing.T) {
	t.Parallel()

	runner := &noopRunner{}
	svc, tracker := newResultTestService(t, runner)
	slug := buildSlug(t)
	cleanupSlug(t, svc.store, slug)

	// An edit on an already-valid site where the agent only reads: the
	// 2026-08-25 incident shape. Lint passes, the run completes, and the
	// verdict must say — explicitly — that nothing changed.
	ctx := context.Background()
	err := svc.store.Write(ctx, slug, "index.html", validIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed index.html: %v", err)
	}

	svc.Start(Params{
		Slug:       slug,
		Prompt:     "make this line white",
		UserPrompt: "make this line white",
		LogKey:     "edit",
		Template:   templates.Get("blank"),
	})
	if status := waitForTerminal(t, tracker, slug, 30*time.Second); status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}

	results := resultEvents(t, tracker, slug)
	if len(results) != 1 {
		t.Fatalf("result events = %d, want exactly 1", len(results))
	}
	if len(results[0].Files) != 0 {
		t.Fatalf("result files = %v, want empty (nothing changed)", results[0].Files)
	}
}

func TestService_Start_MachineryRunsEmitNoResult(t *testing.T) {
	t.Parallel()

	runner := &scriptedRunner{bodies: []string{validIndexHTML}}
	svc, tracker := newResultTestService(t, runner)
	slug := buildSlug(t)
	cleanupSlug(t, svc.store, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "internal pass",
		LogKey:       "relint",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})
	if status := waitForTerminal(t, tracker, slug, 30*time.Second); status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}

	if results := resultEvents(t, tracker, slug); len(results) != 0 {
		t.Fatalf("machinery run emitted %d result event(s), want 0", len(results))
	}
}
