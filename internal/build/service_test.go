package build

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/lint"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
	"github.com/jtarchie/topbanana/internal/templates"
)

// --- Pure unit tests (no MinIO required) ------------------------------------

func TestNormalizeDomain(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "example.com", want: "example.com"},
		{in: "  Example.COM  ", want: "example.com"},
		{in: "example.com:443", want: "example.com"},
		{in: "EXAMPLE.com:80", want: "example.com"},
		{in: "sub.domain.co.uk", want: "sub.domain.co.uk"},
		{in: "", wantErr: true},
		{in: "   ", wantErr: true},
		{in: "no-dot", wantErr: true},
		{in: "has space.com", wantErr: true},
		{in: "trailing/slash.com", wantErr: true},
		{in: "https://example.com", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			t.Parallel()
			got, err := NormalizeDomain(c.in)
			if c.wantErr {
				if err == nil {
					t.Errorf("expected error for %q, got %q", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Errorf("NormalizeDomain(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestLintFixPrompt_FormatsEachError(t *testing.T) {
	t.Parallel()

	errs := []lint.Error{
		{File: "index.html", Message: "missing daisyui"},
		{File: "about.html", Message: "broken link"},
	}
	got := LintFixPrompt(errs)
	if !strings.Contains(got, "Fix these issues in the site:") {
		t.Errorf("prompt missing header: %q", got)
	}
	// The edit-in-place guardrail must be present so the agent reads files
	// before editing instead of regenerating them from the error text alone
	// (the failure mode that wiped a site during relint).
	for _, want := range []string{"read_file", "in place", "do not rewrite"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing guardrail phrase %q in:\n%s", want, got)
		}
	}
	for _, want := range []string{"index.html: missing daisyui", "about.html: broken link"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q in:\n%s", want, got)
		}
	}
}

func TestSplitFilesByKind(t *testing.T) {
	t.Parallel()

	pages, assets := SplitFilesByKind([]string{
		"index.html",
		"about.html",
		"assets/logo.png",
		"assets/hero.jpg",
		".buildabear.json",
		"functions/submit.js",
		"unknown.txt",
	})
	wantPages := []string{"index.html", "about.html"}
	wantAssets := []string{"assets/logo.png", "assets/hero.jpg"}

	if !equalSlice(pages, wantPages) {
		t.Errorf("pages = %v, want %v", pages, wantPages)
	}
	if !equalSlice(assets, wantAssets) {
		t.Errorf("assets = %v, want %v", assets, wantAssets)
	}
}

func TestEditPrompt_BranchesByPageAndSelection(t *testing.T) {
	t.Parallel()

	t.Run("site-wide", func(t *testing.T) {
		got := EditPrompt("add a footer", "", "")
		if !strings.Contains(got, "multi-page site") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("per-page", func(t *testing.T) {
		got := EditPrompt("tweak hero", "index.html", "")
		if !strings.Contains(got, "'index.html'") {
			t.Errorf("got %q", got)
		}
	})
	t.Run("per-selection", func(t *testing.T) {
		got := EditPrompt("make this bigger", "index.html", "<h1>hi</h1>")
		if !strings.Contains(got, "<h1>hi</h1>") || !strings.Contains(got, "index.html") {
			t.Errorf("got %q", got)
		}
	})
}

func TestPagesNamedInPrompt(t *testing.T) {
	t.Parallel()

	pages := []string{"index.html", "about.html", "pricing.html"}
	cases := []struct {
		prompt string
		want   []string
	}{
		{"update About page", []string{"about.html"}},
		{"redo pricing.html and index.html", []string{"pricing.html", "index.html"}},
		{"nothing to match here", nil},
		{"", nil},
		// "home" doesn't match anything in the page list — must not invent.
		{"go home", nil},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			t.Parallel()
			got := pagesNamedInPrompt(pages, c.prompt)
			if !equalSlice(got, c.want) {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// --- Integration tests against MinIO + stub Runner --------------------------

// validIndexHTML is HTML that passes every lint check in package lint
// (parse, design substrate, no broken links). Pieced together to mirror the
// stubIndexHTML used by the server-side e2e tests.
const validIndexHTML = `<!DOCTYPE html>
<html lang="en" data-theme="cupcake">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Test</title>
<meta name="description" content="A test page.">
<link rel="stylesheet" href="/app.css">
</head>
<body>
<main class="p-6"><h1>Hello</h1></main>
</body>
</html>`

// brokenIndexHTML triggers a lint error: every design-substrate piece is
// present (so we don't fail on substrate) but it contains a relative link
// to a page that doesn't exist in the bucket. checkHTMLLinks reports that.
const brokenIndexHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Broken</title>
<link rel="stylesheet" href="/app.css">
</head>
<body><a href="missing.html">go nowhere</a></body>
</html>`

// scriptedRunner is a Runner that returns content from a queue. Each Run
// call writes the next entry's bytes to index.html. Describe returns a
// fixed payload. Test seam for build/edit flows that need a deterministic
// agent without an LLM.
type scriptedRunner struct {
	bodies   []string
	calls    atomic.Int32
	describe agent.SiteDescription
	runDelay time.Duration // optional pause to exercise timeout path
	usage    agent.Usage   // returned from every Run call (zero value is fine)
}

func (r *scriptedRunner) Run(ctx context.Context, s *store.Store, req RunRequest, emit func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	slug := req.Slug
	idx := int(r.calls.Add(1)) - 1
	if r.runDelay > 0 {
		select {
		case <-time.After(r.runDelay):
		case <-ctx.Done():
			return r.usage, ctx.Err() //nolint:wrapcheck
		}
	}
	body := validIndexHTML
	if idx < len(r.bodies) {
		body = r.bodies[idx]
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "/index.html"})
	err := s.Write(ctx, slug, "index.html", body, "text/html; charset=utf-8", nil)
	if err != nil {
		return r.usage, fmt.Errorf("scriptedRunner write: %w", err)
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseDone, Path: "/index.html"})
	return r.usage, nil
}

func (r *scriptedRunner) Describe(_ context.Context, _ *store.Store, _, _ string) (agent.SiteDescription, error) {
	return r.describe, nil
}

// noopRunner writes nothing — it models an agent turn that produced no tool
// calls (the failure mode where a weaker model answers in prose instead of
// calling write_file). Used to lock in that a build leaving no index.html
// fails loudly rather than reporting success on an empty site.
type noopRunner struct{ calls atomic.Int32 }

func (r *noopRunner) Run(_ context.Context, _ *store.Store, _ RunRequest, _ func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	r.calls.Add(1)
	return agent.Usage{}, nil
}

func (r *noopRunner) Describe(_ context.Context, _ *store.Store, _, _ string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

// TestService_Lint_FlagsMissingIndexHTML is the focused check: a site with HTML
// pages but no index.html must not lint clean. Without checkEntryPoint, a site
// with zero HTML files lints clean and a "successful" build serves nothing.
func TestService_Lint_FlagsMissingIndexHTML(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	svc := NewWithConfig(Config{Store: st})
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	ctx := context.Background()
	// A site with only the meta sidecar (as seedTemplate leaves a blank
	// template) — no index.html at all.
	err := st.Write(ctx, slug, ".topbanana.json", "{}", "application/json", nil)
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	errs := svc.Lint(ctx, slug, templates.Get("blank"))
	if len(errs) == 0 {
		t.Fatal("expected a lint error for a site with no index.html, got none")
	}
	var found bool
	for _, e := range errs {
		if e.File == "index.html" && strings.Contains(e.Message, "entry point") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a missing-index.html entry-point error, got: %v", errs)
	}
}

// TestService_Start_FailsWhenNoIndexHTML locks in the product fix end-to-end: a
// build whose agent never writes index.html (noopRunner) must reach the failed
// terminal state, not completed — even with the check-less blank template.
func TestService_Start_FailsWhenNoIndexHTML(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	runner := &noopRunner{}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "build me something",
		LogKey:       "test.build.noindex",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})

	status := waitForTerminal(t, tracker, slug, 60*time.Second)
	if status != events.StatusFailed {
		t.Fatalf("status = %q, want failed (a build that writes no index.html must fail)", status)
	}
	// Author run + maxLintRetries editor runs, all no-ops, before giving up.
	if got := runner.calls.Load(); got < 2 {
		t.Errorf("Runner.Run calls = %d, want >= 2 (author + at least one retry)", got)
	}
}

func TestService_Start_HappyPathSeedsSkeletonWritesMetaCompletes(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	runner := &scriptedRunner{bodies: []string{validIndexHTML}, describe: agent.SiteDescription{Title: "T", Description: "D"}}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "hello",
		LogKey:       "test.build",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}

	// Author run + polish run. The polish phase fires automatically on initial
	// builds and re-uses the same scripted runner; both calls write valid HTML
	// so neither lint nor polish can fail the build.
	if runner.calls.Load() != 2 {
		t.Errorf("Runner.Run calls = %d, want 2 (author + polish)", runner.calls.Load())
	}
	history := collectHistory(t, tracker, slug)
	var sawPolishing bool
	for _, ev := range history {
		if ev.Type == events.TypeStatus && ev.Status == events.StatusPolishing {
			sawPolishing = true
		}
	}
	if !sawPolishing {
		t.Errorf("expected a status=polishing event in the SSE stream; got %d events", len(history))
	}

	// MetaFile should record template, owner, and description from refreshDescription.
	meta := svc.ReadMeta(context.Background(), slug)
	if meta.Template != "blank" {
		t.Errorf("meta.Template = %q, want blank", meta.Template)
	}
	if meta.OwnerID != "tester@example.com" {
		t.Errorf("meta.OwnerID = %q", meta.OwnerID)
	}
	if meta.Title != "T" || meta.Description != "D" {
		t.Errorf("Describe output not merged into meta: %+v", meta)
	}
}

func TestService_Start_LintRetryFixesAndCompletes(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	// First Run produces a broken page (broken link → lint error). Second
	// Run (the editor retry) writes valid HTML. Build should complete.
	runner := &scriptedRunner{bodies: []string{brokenIndexHTML, validIndexHTML}}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "hello",
		LogKey:       "test.build",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}
	if runner.calls.Load() < 2 {
		t.Errorf("Runner.Run calls = %d, want at least 2 (initial + retry)", runner.calls.Load())
	}

	// Make sure the retry status event fired.
	history := collectHistory(t, tracker, slug)
	var sawRetry bool
	for _, ev := range history {
		if ev.Type == events.TypeStatus && ev.Status == events.StatusRetry {
			sawRetry = true
		}
	}
	if !sawRetry {
		t.Errorf("expected a status=retry event in the SSE stream; got %d events", len(history))
	}
}

// substrateMissingHTML is valid except it lacks the /app.css link, so it
// trips exactly one lint error (KindDesignSubstrate) — the deterministic,
// in-code-fixable kind. The body carries marker text we assert survives.
const substrateMissingHTML = `<!DOCTYPE html>
<html lang="en" data-theme="cupcake">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Keepers</title>
<meta name="description" content="A page that keeps its content.">
</head>
<body>
<main class="p-6"><h1>Keep this heading</h1><p>And this paragraph.</p></main>
</body>
</html>`

// TestService_AutoFix_DesignSubstratePreservesContent locks in the relint
// data-loss fix: a missing-/app.css error is repaired in-code with the
// existing content intact, leaving zero residual errors for the agent.
func TestService_AutoFix_DesignSubstratePreservesContent(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	svc := NewWithConfig(Config{Store: st})
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	ctx := context.Background()
	err := st.Write(ctx, slug, "index.html", substrateMissingHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("seed write: %v", err)
	}

	tmpl := templates.Get("blank")
	errs := svc.Lint(ctx, slug, tmpl)
	if len(errs) == 0 {
		t.Fatalf("expected a design-substrate lint error for a page missing /app.css")
	}

	residual := svc.AutoFix(ctx, slug, errs)
	if len(residual) != 0 {
		t.Errorf("AutoFix residual = %d, want 0 (all deterministically fixable): %v", len(residual), residual)
	}
	if got := svc.Lint(ctx, slug, tmpl); len(got) != 0 {
		t.Errorf("re-lint after AutoFix = %d errors, want 0: %v", len(got), got)
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !strings.Contains(obj.Content, `href="/app.css"`) {
		t.Errorf("AutoFix did not inject the /app.css link:\n%s", obj.Content)
	}
	for _, want := range []string{"Keep this heading", "And this paragraph.", `data-theme="cupcake"`} {
		if !strings.Contains(obj.Content, want) {
			t.Errorf("AutoFix dropped original content %q:\n%s", want, obj.Content)
		}
	}
}

func TestService_Start_MaxLintRetriesExceededFails(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	// Every Run writes the broken page; the lint loop should give up after
	// maxLintRetries and emit a failure.
	runner := &scriptedRunner{bodies: []string{brokenIndexHTML, brokenIndexHTML, brokenIndexHTML, brokenIndexHTML, brokenIndexHTML}}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "hello",
		LogKey:       "test.build",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	got := tracker.Get(slug)
	// The user-facing failure is humanized (HumanizeFailure); the broken
	// relative link maps to the friendly broken-link headline. The raw
	// "lint errors after …" text now lives in the event's Detail field.
	if !strings.Contains(got.Error, "link pointed to a page") {
		t.Errorf("failure message = %q, want friendly broken-link text", got.Error)
	}
}

func TestService_Start_TimeoutFailsWithDeadlineMessage(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	// Runner sleeps past the build timeout; buildAndLint must cancel and
	// surface a "build timed out" failure.
	runner := &scriptedRunner{
		bodies:   []string{validIndexHTML},
		runDelay: 500 * time.Millisecond,
	}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 50 * time.Millisecond,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "hello",
		LogKey:       "test.build",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
	})

	status := waitForTerminal(t, tracker, slug, 5*time.Second)
	if status != events.StatusFailed {
		t.Fatalf("status = %q, want failed", status)
	}
	got := tracker.Get(slug)
	// Humanized: the raw "build timed out after …" maps to the friendly
	// "taking longer than expected" headline (raw kept in event Detail).
	if !strings.Contains(got.Error, "taking longer") {
		t.Errorf("failure message = %q, want friendly timeout text", got.Error)
	}
}

func TestService_WriteMeta_ReadMetaRoundtrip(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	svc := NewWithConfig(Config{Store: st})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	meta := SiteMeta{
		Template:         "blank",
		Created:          time.Now().UTC().Truncate(time.Second),
		Domains:          []string{"example.com"},
		EnablesFunctions: true,
		Title:            "title",
		Description:      "desc",
		OwnerID:          "x@example.com",
	}
	err := svc.WriteMeta(context.Background(), slug, meta)
	if err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}

	got := svc.ReadMeta(context.Background(), slug)
	if got.Template != meta.Template || got.EnablesFunctions != meta.EnablesFunctions {
		t.Errorf("round-trip mismatch: %+v vs %+v", got, meta)
	}
	if len(got.Domains) != 1 || got.Domains[0] != "example.com" {
		t.Errorf("domains lost: %+v", got.Domains)
	}
	if got.OwnerID != meta.OwnerID {
		t.Errorf("OwnerID lost: %q", got.OwnerID)
	}
}

func TestService_ReadMeta_MissingReturnsZeroValue(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	svc := NewWithConfig(Config{Store: st})
	got := svc.ReadMeta(context.Background(), "no-such-slug-"+buildSuffix())
	if got.Template != "" || got.OwnerID != "" {
		t.Errorf("missing meta returned non-zero: %+v", got)
	}
}

// Sites created before a rebrand store their sidecar at a legacy name
// (.bloomhollow.json predates Top Banana, .buildabear.json predates
// Bloomhollow). ReadMeta must fall through to each when the canonical
// .topbanana.json is absent, otherwise existing sites silently lose template
// id, domains, and function flags after the upgrade.
func TestService_ReadMeta_FallsBackToLegacySidecar(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)

	svc := NewWithConfig(Config{Store: st})

	for _, legacyFile := range legacyMetaFiles {
		t.Run(legacyFile, func(t *testing.T) {
			slug := buildSlug(t)
			cleanupSlug(t, st, slug)

			legacy := SiteMeta{
				Template:         "blank",
				Domains:          []string{"legacy.example.com"},
				EnablesFunctions: true,
				OwnerID:          "legacy@example.com",
			}
			body, err := json.Marshal(legacy)
			if err != nil {
				t.Fatalf("marshal legacy meta: %v", err)
			}
			err = st.Write(context.Background(), slug, legacyFile, string(body), "application/json", nil)
			if err != nil {
				t.Fatalf("seed legacy sidecar: %v", err)
			}

			got := svc.ReadMeta(context.Background(), slug)
			if got.Template != legacy.Template {
				t.Errorf("legacy fallback: Template=%q, want %q", got.Template, legacy.Template)
			}
			if got.OwnerID != legacy.OwnerID {
				t.Errorf("legacy fallback: OwnerID=%q, want %q", got.OwnerID, legacy.OwnerID)
			}
			if !got.EnablesFunctions {
				t.Errorf("legacy fallback: EnablesFunctions lost")
			}
			if len(got.Domains) != 1 || got.Domains[0] != "legacy.example.com" {
				t.Errorf("legacy fallback: Domains=%+v", got.Domains)
			}

			// New writes go to the canonical name; the legacy file is left in
			// place as belt-and-suspenders for rollback, but ReadMeta now
			// prefers the new.
			updated := got
			updated.Title = "after migration"
			err = svc.WriteMeta(context.Background(), slug, updated)
			if err != nil {
				t.Fatalf("WriteMeta: %v", err)
			}
			obj, err := st.Read(context.Background(), slug, MetaFile)
			if err != nil || obj.Content == "" {
				t.Fatalf("WriteMeta did not persist to MetaFile (%s): err=%v content=%q", MetaFile, err, obj.Content)
			}
		})
	}
}

// --- helpers ----------------------------------------------------------------

// minioStoreForBuild returns the test store: in-memory by default, S3/Minio
// when AWS_ENDPOINT_URL + S3_BUCKET are set (see internal/storetest). Kept as a
// named wrapper so the many call sites read unchanged.
func minioStoreForBuild(t *testing.T) *store.Store {
	t.Helper()
	return storetest.New(t, 0)
}

func buildSuffix() string { return strconv.FormatInt(time.Now().UnixNano(), 36) }

func buildSlug(t *testing.T) string {
	t.Helper()
	return "buildtest-" + buildSuffix()
}

// cleanupSlug registers a t.Cleanup that drops every file under the slug
// prefix. The store has no native DeleteSlug — list + delete is the only
// hammer available — but at MinIO speed it costs nothing.
func cleanupSlug(t *testing.T, s *store.Store, slug string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		files, _ := s.List(ctx, slug)
		for _, f := range files {
			_ = s.Delete(ctx, slug, f)
		}
	})
}

func waitForTerminal(t *testing.T, tracker *events.Tracker, slug string, deadline time.Duration) string {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		s := tracker.Get(slug)
		if s != nil && (s.Status == events.StatusCompleted || s.Status == events.StatusFailed) {
			return s.Status
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("build did not reach terminal status within %s", deadline)
	return ""
}

func collectHistory(t *testing.T, tracker *events.Tracker, slug string) []events.Event {
	t.Helper()
	history, ch, _ := tracker.Subscribe(slug)
	t.Cleanup(func() { tracker.Unsubscribe(slug, ch) })
	return history
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Verify that SiteMeta JSON tags don't drift — would silently break Read/WriteMeta.
func TestSiteMeta_JSONRoundtrip(t *testing.T) {
	t.Parallel()
	original := SiteMeta{
		Template: "blank",
		Created:  time.Now().UTC().Truncate(time.Second),
		Domains:  []string{"a.com", "b.com"},
		OwnerID:  "x@example.com",
		Title:    "t",
	}
	body, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got SiteMeta
	err = json.Unmarshal(body, &got)
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Template != original.Template || len(got.Domains) != 2 || got.OwnerID != original.OwnerID {
		t.Errorf("roundtrip mismatch: %+v vs %+v", got, original)
	}
}

// Sanity: maxLintRetries is the documented value. A regression that
// lowered it would silently change retry behaviour.
func TestMaxLintRetriesConstant(t *testing.T) {
	t.Parallel()
	if maxLintRetries != 3 {
		t.Errorf("maxLintRetries = %d, want 3 (per package contract)", maxLintRetries)
	}
}

// Defensive: ensure ErrNotImplemented-style paths the test setup might
// rely on actually exist. If lint errors stop returning anything from the
// broken HTML, our retry test would silently succeed on the first turn.
func TestBrokenHTMLActuallyFailsLint(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := st.Write(ctx, slug, "index.html", brokenIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write broken: %v", err)
	}
	errs := lint.App(ctx, st, slug, templates.Get("blank"))
	if len(errs) == 0 {
		t.Fatalf("expected lint errors for broken HTML; got 0")
	}
}

// TestApplyAutoFixers exercises the deterministic heart of autoFixLint without
// a store: a page missing both the /app.css link and the viewport meta is
// repaired in one pass, a single requested kind fixes only that, and a complete
// page changes nothing.
func TestApplyAutoFixers(t *testing.T) {
	t.Parallel()

	bare := `<!DOCTYPE html><html><head><title>x</title></head><body></body></html>`

	t.Run("both kinds fixed in one pass", func(t *testing.T) {
		out, done := applyAutoFixers(bare, map[lint.Kind]bool{
			lint.KindDesignSubstrate: true,
			lint.KindMobileViewport:  true,
		})
		if !done[lint.KindDesignSubstrate] || !done[lint.KindMobileViewport] {
			t.Fatalf("expected both kinds fixed, got %v", done)
		}
		if !strings.Contains(out, `href="/app.css"`) || !strings.Contains(out, "width=device-width") {
			t.Errorf("both tags should be injected:\n%s", out)
		}
	})

	t.Run("only the requested kind is applied", func(t *testing.T) {
		out, done := applyAutoFixers(bare, map[lint.Kind]bool{lint.KindMobileViewport: true})
		if done[lint.KindDesignSubstrate] {
			t.Error("design substrate must not be fixed when not requested")
		}
		if !done[lint.KindMobileViewport] || strings.Contains(out, `href="/app.css"`) {
			t.Errorf("only the viewport should be injected:\n%s", out)
		}
	})

	t.Run("complete page changes nothing", func(t *testing.T) {
		complete := `<html><head><meta name="viewport" content="width=device-width, initial-scale=1"><link rel="stylesheet" href="/app.css"></head><body></body></html>`
		_, done := applyAutoFixers(complete, map[lint.Kind]bool{
			lint.KindDesignSubstrate: true,
			lint.KindMobileViewport:  true,
		})
		if len(done) != 0 {
			t.Errorf("nothing should change on a complete page, got %v", done)
		}
	})
}

func TestShouldPolishEdit(t *testing.T) {
	t.Parallel()

	cases := []struct {
		prompt string
		want   bool
	}{
		{"polish the about page", true},
		{"Please polish the hero", true},
		{"tighten up the spacing", true},
		{"refine the typography", true},
		{"clean up the navbar", true},
		{"clean it up", true},
		{"change the hero copy", false},
		{"add a new section about pricing", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.prompt, func(t *testing.T) {
			t.Parallel()
			got := shouldPolishEdit(c.prompt)
			if got != c.want {
				t.Errorf("shouldPolishEdit(%q) = %v, want %v", c.prompt, got, c.want)
			}
		})
	}
}

// TestEmbeddedPromptsNonEmpty guards against a sibling *_prompt.md file being
// emptied or accidentally truncated. //go:embed errors at compile time if a
// file is missing, but a zero-byte file would slip through and the LLM would
// silently get an empty instruction.
func TestEmbeddedPromptsNonEmpty(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"polishPrompt":           polishPrompt,
		"editSitePromptFmt":      editSitePromptFmt,
		"editPagePromptFmt":      editPagePromptFmt,
		"editSelectionPromptFmt": editSelectionPromptFmt,
	} {
		if body == "" {
			t.Errorf("%s embedded prompt is empty — was the .md file emptied?", name)
		}
	}
}

// polishPrompt is the user-prompt fired by PolishPass. It must keep the
// read-before-edit guardrail (so the agent does not rewrite pages from the
// prompt alone — the same failure mode lintFixGuardrail guards against) and
// it must reference Top Banana's design substrate so the polish stays on
// theme tokens rather than custom CSS.
func TestPolishPrompt_ReferencesCoreConstraints(t *testing.T) {
	t.Parallel()

	must := []string{
		"polishing",         // identifies the phase
		"in place",          // edit-in-place contract
		"do not rewrite",    // forbid blind rewrites
		`"done"`,            // explicit stop sentinel
		"focus-visible",     // interaction-state requirement
		"DaisyUI",           // self-hosted substrate cue
		"text-base-content", // theme token cue
	}
	for _, want := range must {
		if !strings.Contains(polishPrompt, want) {
			t.Errorf("polishPrompt missing %q", want)
		}
	}
}

// TestService_Start_EditSkipsPolishByDefault locks in the cost guard: an
// edit whose prompt does not opt into polish runs only the author turn —
// no second editor.Run call for a polish pass, no StatusPolishing event.
//
// Not parallel: each Service test spins its own MinIO HTTP transport (idle
// connections live until the SDK's keep-alive timeout). Running the polish
// integration tests serially keeps the package's process-wide goleak check
// from observing leftover persistConn goroutines at test-suite exit.
func TestService_Start_EditSkipsPolishByDefault(t *testing.T) {
	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	runner := &scriptedRunner{bodies: []string{validIndexHTML}}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:     slug,
		Prompt:   "change the hero copy",
		LogKey:   "test.edit.no_polish",
		Template: templates.Get("blank"),
		// SeedSkeleton omitted → this is an edit.
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}
	if got := runner.calls.Load(); got != 1 {
		t.Errorf("Runner.Run calls = %d, want 1 (author only — edit without polish keyword)", got)
	}
	history := collectHistory(t, tracker, slug)
	for _, ev := range history {
		if ev.Type == events.TypeStatus && ev.Status == events.StatusPolishing {
			t.Errorf("unexpected status=polishing event on a non-opt-in edit")
		}
	}
}

// TestService_Start_EditPolishesOnOptIn is the inverse: when the edit
// prompt asks for polish ("polish the about page"), the polish phase
// fires after the author run. Not parallel — same goleak reason as the
// sibling polish test.
func TestService_Start_EditPolishesOnOptIn(t *testing.T) {
	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	runner := &scriptedRunner{bodies: []string{validIndexHTML, validIndexHTML}}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:     slug,
		Prompt:   "polish the about page",
		LogKey:   "test.edit.opt_in_polish",
		Template: templates.Get("blank"),
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed", status)
	}
	if got := runner.calls.Load(); got != 2 {
		t.Errorf("Runner.Run calls = %d, want 2 (author + polish on opt-in)", got)
	}
	history := collectHistory(t, tracker, slug)
	var sawPolishing bool
	for _, ev := range history {
		if ev.Type == events.TypeStatus && ev.Status == events.StatusPolishing {
			sawPolishing = true
		}
	}
	if !sawPolishing {
		t.Errorf("expected status=polishing event on an opt-in edit; got %d events", len(history))
	}
}

// failOnNthRunner writes validIndexHTML on every Run call except the n-th
// (1-indexed), where it returns an error. Used to assert that a polish-turn
// failure does not promote to a build-level failure — the user still sees
// status=completed.
type failOnNthRunner struct {
	calls      atomic.Int32
	failOnCall int32
	err        error
}

func (r *failOnNthRunner) Run(ctx context.Context, s *store.Store, req RunRequest, _ func(events.Event), _ *events.Tracker) (agent.Usage, error) {
	n := r.calls.Add(1)
	if n == r.failOnCall {
		return agent.Usage{}, r.err
	}
	err := s.Write(ctx, req.Slug, "index.html", validIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		return agent.Usage{}, fmt.Errorf("failOnNthRunner write: %w", err)
	}
	return agent.Usage{}, nil
}

func (r *failOnNthRunner) Describe(_ context.Context, _ *store.Store, _, _ string) (agent.SiteDescription, error) {
	return agent.SiteDescription{}, nil
}

// TestService_Start_PolishFailureNonFatal locks in the best-effort contract:
// if the polish turn errors, the build still reaches status=completed
// (matching OptimizeCSS / refreshDescription failure behaviour). Not
// parallel — same goleak reason as the sibling polish test.
func TestService_Start_PolishFailureNonFatal(t *testing.T) {
	st := minioStoreForBuild(t)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	// Author run (call 1) writes a valid page → lint passes. Polish run
	// (call 2) returns an error. Build should still complete.
	runner := &failOnNthRunner{failOnCall: 2, err: errors.New("simulated polish failure")}
	svc := NewWithConfig(Config{
		Store:        st,
		Events:       tracker,
		Runner:       runner,
		BuildTimeout: 30 * time.Second,
	})

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	svc.Start(Params{
		Slug:         slug,
		Prompt:       "hello",
		LogKey:       "test.build.polish_fail",
		Template:     templates.Get("blank"),
		SeedSkeleton: true,
		OwnerID:      "tester@example.com",
	})

	status := waitForTerminal(t, tracker, slug, 30*time.Second)
	if status != events.StatusCompleted {
		t.Fatalf("status = %q, want completed (polish failure must not fail the build)", status)
	}
	if got := runner.calls.Load(); got != 2 {
		t.Errorf("Runner.Run calls = %d, want 2 (author + failed polish)", got)
	}
}

// Defensive: the valid HTML fixture must actually pass lint, otherwise
// the retry test would loop until max-retries and fail for the wrong reason.
func TestValidHTMLPassesLint(t *testing.T) {
	t.Parallel()

	st := minioStoreForBuild(t)
	ctx := context.Background()
	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	err := st.Write(ctx, slug, "index.html", validIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		t.Fatalf("write valid: %v", err)
	}
	errs := lint.App(ctx, st, slug, templates.Get("blank"))
	if len(errs) != 0 {
		t.Fatalf("valid fixture failed lint: %+v", errs)
	}
}
