package build

// Opt-in integration tests that drive the REAL agent loop end-to-end against a
// local LLM (LM Studio, default model google/gemma-4-12b). Unlike every other
// agent test in this repo — which inject a stub Runner — these exercise the
// actual prompt assembly, the ADK tool loop (write_file/read_file/edit_file),
// the lint -> autofix -> editor-retry loop, OptimizeCSS, and the utility-tier
// Describe against a model that genuinely produces the HTML.
//
// They are GATED so they stay out of the default `go test`/`task fmt`/CI run:
// requireLLM skips unless TOPBANANA_LLM_E2E is set AND a MinIO store is
// configured AND LM Studio answers a pre-flight ping. Run them with:
//
//	task test:llm
//
// A 12B model is non-deterministic, so the assertions are structural
// invariants (build completed, index.html non-empty, lint clean, a write_file
// tool call happened), not exact output. Content-shape checks are logged, not
// failed. See internal/build/service_test.go for the reused helpers
// (minioStoreForBuild, buildSlug, cleanupSlug, waitForTerminal, collectHistory).

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"

	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/templates"
)

// llmBuildTimeout caps a single build; local models are slow and turn-heavy
// (rich templates can drive 20+ agent turns, each re-prefilling ~10K tokens),
// so this is generous — a slow local model needs the headroom to converge.
const llmBuildTimeout = 20 * time.Minute

// llmWaitDeadline must exceed llmBuildTimeout so the Service's own context
// deadline produces a clean "build timed out" failure rather than the poller
// in waitForTerminal giving up first.
const llmWaitDeadline = 21 * time.Minute

// llmWarmupTimeout bounds the pre-flight generate. A model that can't answer a
// one-word prompt in this long is unhealthy — skip rather than burn a full
// build timeout on a cold/stuck model (the first inference after a fresh `lms
// load` was observed to hang indefinitely; warming it here avoids that). Wide
// enough to absorb a cold load of a multi-GB model on first request.
const llmWarmupTimeout = 5 * time.Minute

func getenvDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// requireLLM gates the real-model tests. Returns the store, a factory that
// resolves model IDs against the configured LM Studio endpoint, and the model
// ID to use. Skips (never fails) when any prerequisite is missing.
func requireLLM(t *testing.T) (*store.Store, LLMFactory, string) {
	t.Helper()

	if os.Getenv("TOPBANANA_LLM_E2E") == "" {
		t.Skip("set TOPBANANA_LLM_E2E=1 (+ AWS_ENDPOINT_URL/S3_BUCKET and a running LM Studio) to run real-LLM integration tests")
	}

	st := minioStoreForBuild(t)
	if st == nil {
		t.Skip("set AWS_ENDPOINT_URL + S3_BUCKET to run real-LLM integration tests")
	}

	apiKey := getenvDefault("LLM_API_KEY", "lm-studio")
	baseURL := getenvDefault("LLM_BASE_URL", "http://localhost:1234/v1")
	modelID := getenvDefault("LLM_MODEL", "lmstudio/google/gemma-4-12b")
	_, modelName := model.SplitModel(modelID)

	// Pre-flight: a down LM Studio should skip in seconds, not hang until the
	// multi-minute build timeout.
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(baseURL + "/models")
	if err != nil {
		t.Skipf("LM Studio not reachable at %s: %v", baseURL, err)
	}
	_ = resp.Body.Close()

	// Warm the model with a tiny generate. The first inference after a fresh
	// `lms load` was observed to hang for the full build timeout; warming here
	// both forces that cost out of the timed build and turns an unhealthy model
	// into a fast skip instead of a 12-minute false failure.
	warmUpModel(t, baseURL, apiKey, modelName)

	factory := func(_ context.Context, id string) (adkmodel.LLM, error) {
		provider, name := model.SplitModel(id)
		return model.Resolve(provider, name, apiKey, baseURL)
	}
	return st, factory, modelID
}

// warmUpModel issues a minimal chat completion so the first real build doesn't
// pay cold-start latency, and skips the suite if the model can't respond within
// llmWarmupTimeout.
func warmUpModel(t *testing.T, baseURL, apiKey, modelName string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model":      modelName,
		"messages":   []map[string]string{{"role": "user", "content": "Reply with the single word OK."}},
		"max_tokens": 8,
		"stream":     false,
	})
	ctx, cancel := context.WithTimeout(context.Background(), llmWarmupTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build warmup request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{Timeout: llmWarmupTimeout}).Do(req)
	if err != nil {
		t.Skipf("model %q did not warm up within %s (unreachable/stuck): %v", modelName, llmWarmupTimeout, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		t.Skipf("warmup generate for %q returned HTTP %d: %s", modelName, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
}

// newLLMService wires a build Service to the real model factory. The author
// tier is the only one that must be set; editor/utility fall back to it.
func newLLMService(t *testing.T, st *store.Store, factory LLMFactory, modelID string) (*Service, *events.Tracker) {
	t.Helper()
	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)
	svc := NewWithConfig(Config{
		Store:        st,
		TierMap:      model.TierMap{model.TierAuthor: modelID},
		LLMFactory:   factory,
		Events:       tracker,
		BuildTimeout: llmBuildTimeout,
		RecordEdit:   false,                     // transcript capture isn't under test
		TailwindCLI:  os.Getenv("TAILWIND_CLI"), // empty -> PATH lookup; OptimizeCSS no-ops if absent
	})
	return svc, tracker
}

func mustTemplate(t *testing.T, id string) *templates.SiteTemplate {
	t.Helper()
	tmpl := templates.Get(id)
	if tmpl == nil {
		t.Fatalf("template %q not found", id)
	}
	return tmpl
}

// assertBuiltSite checks the invariants every real-model build must satisfy no
// matter what text the model emits. Invariants hard-fail; content-shape checks
// are logged (model phrasing varies — promote to hard only if they prove
// stable). Safe to call after waitForTerminal returns: Service.Start emits the
// terminal "completed" event last, after OptimizeCSS and Describe, so the
// site's files are final by then.
func assertBuiltSite(t *testing.T, st *store.Store, svc *Service, tracker *events.Tracker, slug string, tmpl *templates.SiteTemplate, status string) {
	t.Helper()
	ctx := context.Background()

	if status != events.StatusCompleted {
		errDetail := ""
		if s := tracker.Get(slug); s != nil {
			errDetail = s.Error
		}
		t.Fatalf("build status = %q, want %q (error: %s)", status, events.StatusCompleted, errDetail)
	}

	obj, err := st.Read(ctx, slug, "index.html")
	if err != nil {
		t.Fatalf("read index.html: %v", err)
	}
	if strings.TrimSpace(obj.Content) == "" {
		t.Fatal("index.html is empty after a completed build")
	}

	// Independent post-build lint: confirms the /app.css link, relative-link
	// integrity, HTML validity, and any template invariants are clean — and
	// that OptimizeCSS's page rewrite didn't break anything.
	if errs := svc.Lint(ctx, slug, tmpl); len(errs) != 0 {
		t.Fatalf("lint after build = %d error(s), want 0: %v", len(errs), errs)
	}

	if !sawWriteFile(t, tracker, slug) {
		t.Fatal("no write_file tool event in build history — the model never exercised the real tool surface")
	}

	// Soft structural sanity.
	if !strings.Contains(strings.ToLower(obj.Content), "<h1") {
		t.Logf("warning: index.html has no <h1 heading")
	}
	if title := svc.ReadMeta(ctx, slug).Title; title == "" {
		t.Logf("warning: site meta Title is empty (utility-tier Describe produced nothing)")
	}
}

func sawWriteFile(t *testing.T, tracker *events.Tracker, slug string) bool {
	t.Helper()
	for _, ev := range collectHistory(t, tracker, slug) {
		if ev.Type == events.TypeTool && ev.Tool == "write_file" {
			return true
		}
	}
	return false
}

// TestLLM_Build_Smoke is the basic smoke test: seed a skeleton-backed template,
// let the model fill it in, lint/retry to clean, compile CSS, describe. A
// skeleton template seeds an index.html the model edits — far more reliable for
// local models than the blank template, which requires the model to initiate
// write_file from scratch (weaker models often answer in prose instead, leaving
// an empty build). link-in-bio is the leanest skeleton (one short single-column
// page), so a slow local model converges in the fewest agent turns.
func TestLLM_Build_Smoke(t *testing.T) {
	st, factory, modelID := requireLLM(t)
	svc, tracker := newLLMService(t, st, factory, modelID)

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	tmpl := mustTemplate(t, "link-in-bio")
	svc.Start(Params{
		Slug:         slug,
		Prompt:       "Build a link-in-bio page for a musician named Kira Vale with links to her latest single, tour dates, and social profiles.",
		LogKey:       "test.llm.smoke",
		Template:     tmpl,
		SeedSkeleton: true,
		OwnerID:      "llm-e2e@example.com",
	})

	status := waitForTerminal(t, tracker, slug, llmWaitDeadline)
	assertBuiltSite(t, st, svc, tracker, slug, tmpl, status)
}

// TestLLM_Build_Template additionally exercises template prompt-addendum
// layering and checkTemplateInvariants (landing-page requires an <h1>).
func TestLLM_Build_Template(t *testing.T) {
	st, factory, modelID := requireLLM(t)
	svc, tracker := newLLMService(t, st, factory, modelID)

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	tmpl := mustTemplate(t, "landing-page")
	svc.Start(Params{
		Slug:         slug,
		Prompt:       "Create a launch page for a productivity app called FocusFlow with a headline, three feature highlights, and a call-to-action to join the waitlist.",
		LogKey:       "test.llm.template",
		Template:     tmpl,
		SeedSkeleton: true,
		OwnerID:      "llm-e2e@example.com",
	})

	status := waitForTerminal(t, tracker, slug, llmWaitDeadline)
	assertBuiltSite(t, st, svc, tracker, slug, tmpl, status)
}

// TestLLM_Build_ThenEdit builds a site, then edits it (SeedSkeleton:false drives
// the isEdit path), asserting the edited site still completes and lints clean.
func TestLLM_Build_ThenEdit(t *testing.T) {
	st, factory, modelID := requireLLM(t)
	svc, tracker := newLLMService(t, st, factory, modelID)

	slug := buildSlug(t)
	cleanupSlug(t, st, slug)

	tmpl := mustTemplate(t, "resume")

	// Initial build.
	svc.Start(Params{
		Slug:         slug,
		Prompt:       "Create a resume site for a backend engineer named Devin Park with a summary, work experience, and a skills section.",
		LogKey:       "test.llm.edit.build",
		Template:     tmpl,
		SeedSkeleton: true,
		OwnerID:      "llm-e2e@example.com",
	})
	status := waitForTerminal(t, tracker, slug, llmWaitDeadline)
	assertBuiltSite(t, st, svc, tracker, slug, tmpl, status)

	// Drop the first build's event history so the edit's write_file assertion
	// sees only the edit's events.
	tracker.Forget(slug)

	// Edit pass. EditPrompt mirrors how the server composes a site-wide edit.
	userMsg := "Change the name in the heading to Devin J. Park."
	svc.Start(Params{
		Slug:         slug,
		Prompt:       EditPrompt(userMsg, ""),
		UserPrompt:   userMsg,
		LogKey:       "test.llm.edit.edit",
		Template:     tmpl,
		SeedSkeleton: false,
		OwnerID:      "llm-e2e@example.com",
	})
	status = waitForTerminal(t, tracker, slug, llmWaitDeadline)
	assertBuiltSite(t, st, svc, tracker, slug, tmpl, status)
}
