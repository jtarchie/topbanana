package build

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/store"
)

// The two prompts from the incident this feature exists for: the user asked
// twice, two and a quarter minutes apart, in different words for the same
// thing. Recognising that pair is the whole job.
const (
	incidentPrompt1 = "Make the tools images larger. They should be about 1/3 to 1/2 the width of the container they're in."
	incidentPrompt2 = "Make the images bigger by scaling them up in size. They should be 1/3 to 1/2 the whole width of the column they're in."
)

func TestRepeatedRun_RecognisesTheIncidentRestatement(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 21, 19, 23, 6, 0, time.UTC)
	runs := []PriorRun{{
		At:     now.Add(-2*time.Minute - 17*time.Second),
		Prompt: incidentPrompt1,
		Files:  []string{"index.html"},
		Status: "completed",
	}}

	got := repeatedRun(runs, incidentPrompt2, now)
	if got == nil {
		t.Fatal("a near-verbatim restatement two minutes later was not recognised")
	}
	if got.Prompt != incidentPrompt1 {
		t.Errorf("matched the wrong run: %q", got.Prompt)
	}
}

func TestRepeatedRun_Negatives(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 21, 19, 23, 6, 0, time.UTC)
	recent := func(prompt string) []PriorRun {
		return []PriorRun{{At: now.Add(-3 * time.Minute), Prompt: prompt, Files: []string{"index.html"}}}
	}

	cases := []struct {
		name  string
		runs  []PriorRun
		asked string
	}{{
		// Two unrelated requests that happen to share most of their words.
		// The token floor is what keeps short prompts out of this.
		name:  "short prompts are never judged",
		runs:  recent("make the hero bigger"),
		asked: "make the images bigger",
	}, {
		name:  "different request of similar length",
		runs:  recent("add a contact form with name and email fields to the bottom of the page"),
		asked: "change the footer copyright year to 2026 and add a link to our terms",
	}, {
		name:  "same request, but long enough ago to be meant afresh",
		runs:  []PriorRun{{At: now.Add(-4 * time.Hour), Prompt: incidentPrompt1}},
		asked: incidentPrompt2,
	}, {
		name:  "no history at all",
		runs:  nil,
		asked: incidentPrompt2,
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := repeatedRun(c.runs, c.asked, now); got != nil {
				t.Errorf("fired on %q vs %q", c.asked, got.Prompt)
			}
		})
	}
}

func TestFormatRunHistory(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 21, 19, 23, 6, 0, time.UTC)

	t.Run("empty history renders nothing", func(t *testing.T) {
		t.Parallel()
		if got := FormatRunHistory(nil, incidentPrompt2, now); got != "" {
			t.Errorf("want empty, got %q", got)
		}
	})

	t.Run("summarises prior runs and flags the restatement", func(t *testing.T) {
		t.Parallel()
		runs := []PriorRun{
			{At: now.Add(-3 * time.Minute), Prompt: incidentPrompt1, Files: []string{"index.html"}, Status: "completed"},
			{At: now.Add(-50 * time.Hour), Prompt: "build me a studio site with a contact form", Files: []string{"index.html", "thanks.html"}, Status: "completed"},
		}
		got := FormatRunHistory(runs, incidentPrompt2, now)

		for _, want := range []string{
			"3 minutes ago",
			"2 days ago",
			"Make the tools images larger",
			"changed index.html",
			"closely restates",
			"stylesheet",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("history block missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("unrelated request gets history but no restatement callout", func(t *testing.T) {
		t.Parallel()
		runs := []PriorRun{{At: now.Add(-3 * time.Minute), Prompt: incidentPrompt1, Files: []string{"index.html"}}}
		got := FormatRunHistory(runs, "change the footer copyright year to 2026 and add a link to our terms", now)

		if !strings.Contains(got, "Make the tools images larger") {
			t.Errorf("prior run should still be summarised:\n%s", got)
		}
		if strings.Contains(got, "closely restates") {
			t.Errorf("restatement callout fired on an unrelated request:\n%s", got)
		}
	})

	t.Run("a run that changed nothing says so", func(t *testing.T) {
		t.Parallel()
		runs := []PriorRun{{At: now.Add(-5 * time.Minute), Prompt: "make it pop more", Status: "failed"}}
		got := FormatRunHistory(runs, "something else entirely here please", now)
		if !strings.Contains(got, "changed nothing") || !strings.Contains(got, "failed") {
			t.Errorf("want the empty/failed run described:\n%s", got)
		}
	})
}

func TestHumanizeAge(t *testing.T) {
	t.Parallel()

	cases := map[time.Duration]string{
		30 * time.Second:    "just now",
		3 * time.Minute:     "3 minutes ago",
		5 * time.Hour:       "5 hours ago",
		50 * time.Hour:      "2 days ago",
		30 * 24 * time.Hour: "30 days ago",
	}
	for d, want := range cases {
		if got := humanizeAge(d); got != want {
			t.Errorf("humanizeAge(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestRecentRuns_ProjectsUserInitiatedRunsOnly drives the store-backed half:
// the LogKey filter and the FileChanges projection are the parts that would
// silently yield nothing if they drifted.
func TestRecentRuns_ProjectsUserInitiatedRunsOnly(t *testing.T) {
	s := minioStoreForBuild(t)
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, s, slug)
	t.Cleanup(func() {
		rows, _ := editrec.List(ctx, s, slug)
		for _, r := range rows {
			_ = editrec.Delete(ctx, s, r.Key)
		}
	})

	write := func(logKey, prompt, path, before, after string) {
		t.Helper()
		rec := editrec.New(slug, logKey, prompt, "")
		err := s.Write(ctx, slug, path, before, "text/html; charset=utf-8", nil)
		if err != nil {
			t.Fatalf("seed %s: %v", path, err)
		}
		emit := rec.Wrap(ctx, s, slug, nil)
		emit(events.Event{Type: events.TypeTool, Tool: "edit_file", Phase: events.PhaseStart, Path: path})
		err = s.Write(ctx, slug, path, after, "text/html; charset=utf-8", nil)
		if err != nil {
			t.Fatalf("edit %s: %v", path, err)
		}
		emit(events.Event{Type: events.TypeTool, Tool: "edit_file", Phase: events.PhaseDone, Path: path})
		rec.Finish(ctx, s, events.StatusCompleted, nil)
		// Transcript keys carry a nanosecond timestamp; keep them distinct.
		time.Sleep(2 * time.Millisecond)
	}

	write("build", "build me a studio site", "index.html", "<p>a</p>", "<p>b</p>")
	write("relint", "fix these lint errors", "index.html", "<p>b</p>", "<p>c</p>")
	write("edit", incidentPrompt1, "index.html", "<p>c</p>", "<p>d</p>")

	svc := newHistoryService(t, s)
	runs := svc.RecentRuns(ctx, slug, historyRuns)

	if len(runs) != 2 {
		t.Fatalf("got %d runs, want 2 (the relint pass is machinery, not a request): %+v", len(runs), runs)
	}
	if runs[0].Prompt != incidentPrompt1 {
		t.Errorf("runs[0] = %q, want the most recent edit", runs[0].Prompt)
	}
	if !slices.Contains(runs[0].Files, "index.html") {
		t.Errorf("runs[0].Files = %v, want index.html", runs[0].Files)
	}
	if runs[1].Prompt != "build me a studio site" {
		t.Errorf("runs[1] = %q, want the initial build", runs[1].Prompt)
	}
}

func newHistoryService(t *testing.T, s *store.Store) *Service {
	t.Helper()
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	return New(s, nil, tracker, nil)
}
