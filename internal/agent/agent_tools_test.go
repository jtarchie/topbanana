package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/templates"
)

func TestToolGuard(t *testing.T) {
	t.Run("first call allowed", func(t *testing.T) {
		g := &toolGuard{}
		err := g.Allow(toolSignature("write_file", "index.html", "hello"))
		if err != nil {
			t.Fatalf("first call should be allowed, got %v", err)
		}
	})

	t.Run("immediate repeat rejected", func(t *testing.T) {
		g := &toolGuard{}
		sig := toolSignature("write_file", "index.html", "hello")
		err := g.Allow(sig)
		if err != nil {
			t.Fatalf("seed call: %v", err)
		}
		err = g.Allow(sig)
		if err == nil {
			t.Fatal("expected repeat to be rejected")
		}
		if !strings.Contains(err.Error(), "write_file") {
			t.Fatalf("error should name the tool, got %q", err.Error())
		}
	})

	t.Run("rolls over after ring length", func(t *testing.T) {
		g := &toolGuard{}
		sig := toolSignature("write_file", "index.html", "hello")
		err := g.Allow(sig)
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// Fill the ring with distinct sigs so the seeded one rolls out.
		for i := range toolGuardRingLen {
			distinct := toolSignature("write_file", "index.html", fmt.Sprintf("filler-%d", i))
			err = g.Allow(distinct)
			if err != nil {
				t.Fatalf("filler %d: %v", i, err)
			}
		}
		err = g.Allow(sig)
		if err != nil {
			t.Fatalf("seeded sig should be allowed after %d distinct intervening calls, got %v",
				toolGuardRingLen, err)
		}
	})

	t.Run("different paths are independent", func(t *testing.T) {
		g := &toolGuard{}
		a := toolSignature("write_file", "a.html", "hello")
		b := toolSignature("write_file", "b.html", "hello")
		err := g.Allow(a)
		if err != nil {
			t.Fatalf("a: %v", err)
		}
		err = g.Allow(b)
		if err != nil {
			t.Fatalf("b: %v", err)
		}
	})
}

func TestSkeletonSeeds_NilOrEmpty(t *testing.T) {
	if got := SkeletonSeeds(nil); got != nil {
		t.Errorf("nil template: expected nil, got %d seeds", len(got))
	}
	if got := SkeletonSeeds(&templates.SiteTemplate{Skeleton: nil}); got != nil {
		t.Errorf("empty skeleton: expected nil, got %d seeds", len(got))
	}
}

func TestSkeletonSeeds_HTML(t *testing.T) {
	tmpl := &templates.SiteTemplate{Skeleton: map[string]string{
		"index.html": "<html><body>a</body></html>",
		"about.html": "<html><body>b\nc</body></html>",
	}}
	seeds := SkeletonSeeds(tmpl)
	if len(seeds) != 3 {
		t.Fatalf("expected 3 seeds (list + 2 reads), got %d", len(seeds))
	}
	if seeds[0].Name != "list_files" {
		t.Errorf("first seed must be list_files, got %s", seeds[0].Name)
	}
	// Names are sorted, so about.html comes first.
	if seeds[1].Name != "read_file" || seeds[1].Args["path"] != "about.html" {
		t.Errorf("second seed must be read_file(about.html), got %+v", seeds[1])
	}
	if seeds[2].Args["path"] != "index.html" {
		t.Errorf("third seed must be read_file(index.html), got %+v", seeds[2].Args)
	}
	resp := seeds[2].Response
	if resp["content"] == "" || resp["total_lines"] == nil {
		t.Errorf("read_file response missing content/total_lines: %+v", resp)
	}
}

func TestSkeletonSeeds_Functions(t *testing.T) {
	tmpl := &templates.SiteTemplate{Skeleton: map[string]string{
		"index.html":            "<html></html>",
		"functions/submit.js":   "module.exports = function() {}",
		"functions/redirect.js": "module.exports = function() {}",
	}}
	seeds := SkeletonSeeds(tmpl)
	// 1 list_files + 1 read_file + 1 list_functions + 2 read_function = 5
	if len(seeds) != 5 {
		t.Fatalf("expected 5 seeds, got %d: %+v", len(seeds), seeds)
	}

	listFns := findSeed(seeds, "list_functions", nil)
	if listFns == nil {
		t.Fatal("expected a list_functions seed")
	}
	fns, _ := listFns.Response["functions"].([]string)
	if len(fns) != 2 || fns[0] != "redirect" || fns[1] != "submit" {
		t.Errorf("list_functions returned wrong names: %+v", fns)
	}

	readSubmit := findSeed(seeds, "read_function", map[string]any{"name": "submit"})
	if readSubmit == nil {
		t.Fatal("expected a read_function(submit) seed")
	}
	if readSubmit.Response["source"] == "" {
		t.Errorf("read_function(submit) missing source")
	}
}

// findSeed returns the first seed whose Name matches and whose Args contain
// every key/value in match. Args may be nil to match on Name alone.
func findSeed(seeds []SeedToolCall, name string, match map[string]any) *SeedToolCall {
	for i := range seeds {
		if seeds[i].Name != name {
			continue
		}
		ok := true
		for k, v := range match {
			if seeds[i].Args[k] != v {
				ok = false
				break
			}
		}
		if ok {
			return &seeds[i]
		}
	}
	return nil
}

func TestLineCount(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"a\n", 1},
		{"a\nb", 2},
		{"a\nb\n", 2},
		{"\n", 1},
	}
	for _, c := range cases {
		if got := lineCount(c.in); got != c.want {
			t.Errorf("lineCount(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFormatBuildContext_Empty(t *testing.T) {
	if got := formatBuildContext(BuildContext{}); got != "" {
		t.Errorf("zero-value BuildContext should render empty, got %q", got)
	}
}

func TestFormatBuildContext_InitialBuild(t *testing.T) {
	// Saturday, 2026-05-30 — picked so the date format assertion is stable.
	when := time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "http://myapp.localhost:8080",
		IsEdit:  false,
	})
	want := "Build context:\n" +
		"- Today: Saturday, 2026-05-30\n" +
		"- Site: myapp at http://myapp.localhost:8080\n" +
		"- Mode: initial build (skeleton seeded — extend it)"
	if got != want {
		t.Errorf("initial build render mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestFormatBuildContext_FollowUpEdit(t *testing.T) {
	when := time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "https://myapp.topbanana.io",
		IsEdit:  true,
	})
	if !strings.Contains(got, "- Mode: follow-up edit") {
		t.Errorf("edit mode line missing: %s", got)
	}
	if !strings.Contains(got, "edit_file / replace_lines") {
		t.Errorf("edit-mode rendering must nudge toward edit_file / replace_lines: %s", got)
	}
	if !strings.Contains(got, "- Site: myapp at https://myapp.topbanana.io") {
		t.Errorf("production-style site URL missing: %s", got)
	}
}

func TestFormatBuildContext_SlugOnlyNoURL(t *testing.T) {
	// The legacy build.New constructor leaves Domain empty so the runner
	// emits SiteURL="". The block should still render with date + slug.
	when := time.Date(2026, 5, 30, 0, 0, 0, 0, time.UTC)
	got := formatBuildContext(BuildContext{
		Now:     when,
		Slug:    "myapp",
		SiteURL: "",
		IsEdit:  false,
	})
	if !strings.Contains(got, "- Site: myapp\n") {
		t.Errorf("site line should render slug alone when SiteURL is empty: %s", got)
	}
	if strings.Contains(got, " at ") {
		t.Errorf("no \" at <url>\" suffix expected when SiteURL is empty: %s", got)
	}
}

func TestFormatBuildContext_Stable(t *testing.T) {
	// Cache stability: identical inputs must produce byte-identical output.
	// Catches accidental non-determinism (map iteration, time formatting).
	bctx := BuildContext{
		Now:     time.Date(2026, 5, 30, 15, 42, 0, 0, time.UTC),
		Slug:    "myapp",
		SiteURL: "http://myapp.localhost:8080",
		IsEdit:  false,
	}
	a := formatBuildContext(bctx)
	b := formatBuildContext(bctx)
	if a != b {
		t.Errorf("formatBuildContext not deterministic:\na: %q\nb: %q", a, b)
	}
}

func TestFormatTemplateChecks(t *testing.T) {
	cases := []struct {
		name   string
		checks []templates.Check
		want   string
	}{
		{
			name:   "empty",
			checks: nil,
			want:   "",
		},
		{
			name: "skips entries missing file or needles",
			checks: []templates.Check{
				{File: "", MustContain: []string{"<h1"}},
				{File: "index.html", MustContain: nil},
			},
			want: "",
		},
		{
			name: "single check with message",
			checks: []templates.Check{
				{File: "index.html", MustContain: []string{"<h1"}, Message: "landing pages need a clear headline"},
			},
			want: "Your output will be validated against these requirements (the lint loop asserts them after every build):\n- index.html must contain `<h1` — landing pages need a clear headline",
		},
		{
			name: "multi-needle joined with and",
			checks: []templates.Check{
				{File: "index.html", MustContain: []string{"<form", `method="post"`}},
			},
			want: "Your output will be validated against these requirements (the lint loop asserts them after every build):\n- index.html must contain `<form` and `method=\"post\"`",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatTemplateChecks(tc.checks)
			if got != tc.want {
				t.Errorf("formatTemplateChecks = %q\nwant: %q", got, tc.want)
			}
		})
	}
}

// --- ask_user tests ----------------------------------------------------------

func newAskState() *buildState { return newBuildState() }

func TestAskUser_ValidationErrors(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	noEmit := func(events.Event) {}

	cases := []struct {
		name string
		args askUserArgs
		want string
	}{
		{"missing question", askUserArgs{Recommendation: "r", Why: "w"}, "question is required"},
		{"missing recommendation", askUserArgs{Question: "q?", Why: "w"}, "recommendation is required"},
		{"missing why", askUserArgs{Question: "q?", Recommendation: "r"}, "why is required"},
		{"too many options", askUserArgs{Question: "q?", Recommendation: "r", Why: "w", Options: []string{"a", "b", "c", "d", "e"}}, "options must have at most 4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh state per case so questionsAsked never exceeds the cap.
			state := newAskState()
			res, err := invokeAskUser(context.Background(), tc.args, "s", tr, noEmit, state, time.Millisecond)
			if err != nil {
				t.Fatalf("unexpected Go error: %v", err)
			}
			if res.Error == "" {
				t.Fatal("expected Error field to be set, got empty")
			}
			if !strings.Contains(res.Error, tc.want) {
				t.Errorf("Error = %q, want substring %q", res.Error, tc.want)
			}
		})
	}
}

func TestAskUser_CapReached(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()
	// Exhaust the cap.
	for range maxQuestionsPerBuild {
		state.questionsAsked.Add(1)
	}

	args := askUserArgs{Question: "q?", Recommendation: "rec", Why: "because"}
	res, err := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Source != "limit_reached" {
		t.Errorf("Source = %q, want limit_reached", res.Source)
	}
	if res.Answer != "rec" {
		t.Errorf("Answer = %q, want recommendation %q", res.Answer, "rec")
	}
}

func TestAskUser_Timeout(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()

	args := askUserArgs{Question: "q?", Recommendation: "default", Why: "because"}
	// Pass a very short timeout so the test runs in milliseconds.
	res, err := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, 5*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected Go error: %v", err)
	}
	if res.Source != "recommendation_timeout" {
		t.Errorf("Source = %q, want recommendation_timeout", res.Source)
	}
	if res.Answer != "default" {
		t.Errorf("Answer = %q, want %q", res.Answer, "default")
	}
}

func TestAskUser_UserAnswer(t *testing.T) {
	t.Parallel()

	tr := events.NewTracker()
	t.Cleanup(tr.Close)
	tr.Start("s")
	state := newAskState()

	// Subscribe BEFORE invoking so we receive the PhaseAsk event.
	_, subCh, _ := tr.Subscribe("s")

	args := askUserArgs{Question: "Tone?", Recommendation: "warm", Why: "prompt said cozy"}

	resultCh := make(chan askUserResult, 1)
	go func() {
		res, _ := invokeAskUser(context.Background(), args, "s", tr, func(events.Event) {}, state, 5*time.Second)
		resultCh <- res
	}()

	// Wait for the PhaseAsk event on the subscriber channel to get the qid.
	var qid string
	deadline := time.After(500 * time.Millisecond)
	for qid == "" {
		select {
		case ev := <-subCh:
			if ev.Type == events.TypeQuestion && ev.Phase == events.PhaseAsk {
				qid = ev.QuestionID
			}
		case <-deadline:
			t.Fatal("timed out waiting for PhaseAsk event on tracker subscriber")
		}
	}

	tr.Resolve("s", qid, "playful")

	select {
	case res := <-resultCh:
		if res.Source != "user" {
			t.Errorf("Source = %q, want user", res.Source)
		}
		if res.Answer != "playful" {
			t.Errorf("Answer = %q, want playful", res.Answer)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("invokeAskUser did not return after Resolve")
	}
}

func TestTruncate(t *testing.T) {
	t.Parallel()

	if truncate("hello", 10) != "hello" {
		t.Error("short string should pass through unchanged")
	}
	long := strings.Repeat("x", 300)
	out := truncate(long, 200)
	if len([]rune(out)) != 200 {
		t.Errorf("truncated length = %d, want 200", len([]rune(out)))
	}
}
