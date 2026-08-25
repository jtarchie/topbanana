package build

// Opt-in reproduction harness for the 2026-08-25 tinytools incident: three
// consecutive user edits ("make this line white", "remove the word
// occasionally") each ran to FinalStatus "completed" with read-only tool calls
// and ZERO file changes. All three ran under the then-new bring-your-own-CSS
// regime (the site carries site.css) with the prior-run history block appended
// to the user turn — the layers this harness exists to hold accountable.
//
// The fixture under testdata/tinytools/ is a byte-faithful copy of the live
// site at the time of the incident, and the seeded prior-run transcripts
// replay the 2026-08-21 "make the images larger" pair (the attribute-only
// edits that site.css silently overruled — the incident the history block and
// the BYO regime were built in response to).
//
// Gated: skips unless OPENROUTER_API_KEY is set. Run with:
//
//	OPENROUTER_API_KEY=sk-or-... go test ./internal/build -run TestLLMEditRepro -v -count=1
//
// Each edit costs roughly one Haiku run (~60-80K prompt tokens, mostly
// cached). Override the model with REPRO_LLM_MODEL (defaults to the
// production model string).
//
// The teeLLM wrapper captures every text part the model emits — the one
// artifact the production transcript does NOT record, and the only place the
// model explains why it declined to edit.

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"iter"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/jtarchie/topbanana/internal/editrec"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/model"
	"github.com/jtarchie/topbanana/internal/store"
	"github.com/jtarchie/topbanana/internal/storetest"
)

//go:embed testdata/tinytools
var tinytoolsFixture embed.FS

// reproEditTimeout bounds one edit run. Haiku over OpenRouter converges in
// well under a minute; the headroom covers lint-fix retries and the polish
// pass.
const reproEditTimeout = 5 * time.Minute

// The three prompts, verbatim from the production transcripts (curly
// apostrophes included — the page stores the same text as &rsquo; entities,
// which is part of what the agent has to navigate).
var reproPrompts = []struct {
	name   string
	prompt string
	// satisfied reports whether the site now reflects what the user asked
	// for. Only used where the outcome is deterministic; nil means "any
	// agent-editable file changed" is the bar.
	satisfied func(t *testing.T, ctx context.Context, st *store.Store, slug string) bool
}{
	{
		name:   "make_line_white",
		prompt: `Make this line white like the rest of the text in this paragraph. "Maybe you’ll like them, too."`,
	},
	{
		name:   "text_at_top_white",
		prompt: `"We’re two friends who enjoy building simple, easy-to-use tools that solve everyday problems. Maybe you’ll like them, too." This text at the top should all be white.`,
	},
	{
		name:   "remove_occasionally",
		prompt: `Remove the word "occasionally" from this sentence, "JT & Leslie are building Tiny Tools Studio because we love simple, useful, and occasionally delightful software. "`,
		satisfied: func(t *testing.T, ctx context.Context, st *store.Store, slug string) bool {
			t.Helper()
			obj, err := st.Read(ctx, slug, "index.html")
			if err != nil {
				t.Fatalf("read index.html: %v", err)
			}
			return !strings.Contains(obj.Content, "occasionally")
		},
	},
}

// The 2026-08-21 prior-run pair seeded into editrec so EditPromptWithHistory
// renders the same history block the incident runs saw ("4 days ago — ... —
// changed index.html").
var reproPriorPrompts = []string{
	"Make the tools images larger. They should be about 1/3 to 1/2 the width of the container they're in.",
	"Make the images bigger by scaling them up in size. They should be 1/3 to 1/2 the whole width of the column they're in.",
}

func TestLLMEditRepro_TinytoolsNoOp(t *testing.T) {
	apiKey := os.Getenv("OPENROUTER_API_KEY")
	if apiKey == "" {
		t.Skip("set OPENROUTER_API_KEY to run the edit no-op reproduction against a real model")
	}
	modelID := getenvDefault("REPRO_LLM_MODEL", "openrouter/~anthropic/claude-haiku-latest")

	ctx := context.Background()
	st := storetest.New(t, 128)
	slug := "tinytools"

	seedTinytoolsSite(t, ctx, st, slug)
	seedPriorRuns(t, ctx, st, slug)

	texts := &modelTextLog{}
	factory := func(_ context.Context, id string) (adkmodel.LLM, error) {
		provider, name := model.SplitModel(id)
		llm, err := model.Resolve(provider, name, apiKey, "")
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", id, err)
		}
		return teeLLM{LLM: llm, log: texts}, nil
	}
	t.Cleanup(http.DefaultClient.CloseIdleConnections)

	tracker := events.NewTracker()
	t.Cleanup(tracker.Close)

	svc := NewWithConfig(Config{
		Store:           st,
		TierMap:         model.TierMap{model.TierAuthor: modelID},
		LLMFactory:      factory,
		Events:          tracker,
		RecordEdit:      true, // runs 2 and 3 must see run 1 in their history, as in production
		ReasoningEffort: genai.ThinkingLevelHigh,
		BuildTimeout:    reproEditTimeout,
		TailwindCLI:     os.Getenv("TAILWIND_CLI"),
	})
	tmpl := mustTemplate(t, "contact-form")

	for i, tc := range reproPrompts {
		ok := t.Run(tc.name, func(t *testing.T) {
			before := editableSHAs(t, ctx, st, slug)

			// contextcheck: Start intentionally runs detached with its own
			// build-timeout context, same as every production call site.
			svc.Start(Params{ //nolint:contextcheck // see comment.
				Slug:       slug,
				Prompt:     svc.EditPromptWithHistory(ctx, slug, tc.prompt, ""),
				LogKey:     "edit",
				Template:   tmpl,
				Seeds:      svc.EditSeeds(ctx, slug, tc.prompt),
				UserPrompt: tc.prompt,
			})
			status := waitForTerminal(t, tracker, slug, reproEditTimeout+time.Minute)

			for _, txt := range texts.drain() {
				t.Logf("model text: %s", txt)
			}
			if status != events.StatusCompleted {
				errDetail := ""
				if s := tracker.Get(slug); s != nil {
					errDetail = s.Error
				}
				t.Fatalf("edit status = %q, want completed (error: %s)", status, errDetail)
			}

			assertFinalMessagesRecorded(t, ctx, st, slug)

			changed := changedPaths(before, editableSHAs(t, ctx, st, slug))
			t.Logf("files changed: %v", changed)
			if len(changed) == 0 {
				t.Errorf("edit %d (%q): agent completed without changing any file — the incident behaviour", i+1, tc.prompt)
			}
			if tc.satisfied != nil && !tc.satisfied(t, ctx, st, slug) {
				t.Errorf("edit %d (%q): site content does not reflect the requested change", i+1, tc.prompt)
			}

			tracker.Forget(slug)
		})
		if !ok && os.Getenv("REPRO_CONTINUE") == "" {
			// Later runs' history depends on earlier outcomes; once one run
			// reproduces the no-op there is usually no need to pay for the
			// rest. Set REPRO_CONTINUE=1 to run all three regardless.
			t.Log("stopping after first failing edit (set REPRO_CONTINUE=1 to run all three)")
			break
		}
	}
}

// assertFinalMessagesRecorded checks the newest transcript captured the
// model's closing text — the diagnosability hole this incident exposed.
func assertFinalMessagesRecorded(t *testing.T, ctx context.Context, st *store.Store, slug string) {
	t.Helper()
	rows, err := editrec.List(ctx, st, slug)
	if err != nil || len(rows) == 0 {
		return
	}
	tr, err := editrec.Read(ctx, st, rows[0].Key)
	if err != nil {
		return
	}
	t.Logf("transcript recorded %d final message(s)", len(tr.FinalMessages))
	if len(tr.FinalMessages) == 0 {
		t.Error("transcript has no final messages — the diagnosability capture regressed")
	}
}

// seedTinytoolsSite writes the fixture files and the sidecar so the store
// looks exactly like the live site did: contact-form template, its own
// site.css (which flips OwnStylesheets to the BYO regime), functions enabled.
func seedTinytoolsSite(t *testing.T, ctx context.Context, st *store.Store, slug string) {
	t.Helper()
	root := "testdata/tinytools"
	err := fs.WalkDir(tinytoolsFixture, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := tinytoolsFixture.ReadFile(p)
		if err != nil {
			return fmt.Errorf("read fixture %s: %w", p, err)
		}
		rel := strings.TrimPrefix(p, root+"/")
		return st.Write(ctx, slug, rel, string(content), contentTypeFor(rel), nil)
	})
	if err != nil {
		t.Fatalf("seed fixture: %v", err)
	}

	svc := &Service{store: st}
	err = svc.WriteMeta(ctx, slug, SiteMeta{
		Template:         "contact-form",
		Created:          time.Now().UTC().Add(-6 * 24 * time.Hour),
		Title:            "Tiny Tools Studio",
		OwnerID:          "repro@example.com",
		EnablesFunctions: true,
	})
	if err != nil {
		t.Fatalf("write meta: %v", err)
	}

	if sheets := OwnStylesheets(ctx, st, slug); len(sheets) != 1 || sheets[0] != "site.css" {
		t.Fatalf("fixture must select the BYO regime via site.css, got %v", sheets)
	}
}

func contentTypeFor(path string) string {
	switch {
	case strings.HasSuffix(path, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(path, ".png"):
		return "image/png"
	default:
		return "application/octet-stream"
	}
}

// seedPriorRuns writes the 2026-08-21 transcript pair (4 days before the
// incident, well inside historyMaxAge) so RecentRuns surfaces them exactly as
// production did. Each records a real index.html change — the attribute edits
// that site.css overruled.
func seedPriorRuns(t *testing.T, ctx context.Context, st *store.Store, slug string) {
	t.Helper()
	base := time.Now().UTC().Add(-4 * 24 * time.Hour)
	for i, prompt := range reproPriorPrompts {
		startedAt := base.Add(time.Duration(i) * 3 * time.Minute)
		tr := editrec.Transcript{
			Slug:        slug,
			LogKey:      "edit",
			StartedAt:   startedAt,
			FinishedAt:  startedAt.Add(30 * time.Second),
			Model:       "openrouter/~anthropic/claude-haiku-latest",
			Template:    "contact-form",
			UserPrompt:  prompt,
			FinalStatus: "completed",
			FileChanges: []editrec.FileChange{{
				Tool:         "edit_file",
				Path:         "index.html",
				BeforeSize:   1,
				AfterSize:    1,
				BeforeSHA256: fmt.Sprintf("before-%d", i),
				AfterSHA256:  fmt.Sprintf("after-%d", i),
			}},
		}
		body, err := json.Marshal(tr)
		if err != nil {
			t.Fatalf("marshal prior run: %v", err)
		}
		key := editrec.Key(slug, startedAt, "edit")
		err = st.WriteRaw(ctx, key, string(body), "application/json", nil)
		if err != nil {
			t.Fatalf("seed prior run %s: %v", key, err)
		}
	}

	runs := (&Service{store: st}).RecentRuns(ctx, slug, historyRuns)
	if len(runs) != len(reproPriorPrompts) {
		t.Fatalf("seeded %d prior runs, RecentRuns sees %d", len(reproPriorPrompts), len(runs))
	}
}

// editableSHAs snapshots every agent-editable file (excludes the generated
// app.css, sidecars, functions/ — the polish/CSS passes must not read as the
// agent having acted).
func editableSHAs(t *testing.T, ctx context.Context, st *store.Store, slug string) map[string]string {
	t.Helper()
	files, err := st.List(ctx, slug)
	if err != nil {
		t.Fatalf("list %s: %v", slug, err)
	}
	out := map[string]string{}
	for _, f := range EditableFiles(files) {
		obj, err := st.Read(ctx, slug, f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		out[f] = obj.ETag + "|" + strconv.Itoa(len(obj.Content)) + "|" + obj.Content
	}
	return out
}

func changedPaths(before, after map[string]string) []string {
	var out []string
	for p, v := range after {
		if before[p] != v {
			out = append(out, p)
		}
	}
	for p := range before {
		if _, ok := after[p]; !ok {
			out = append(out, p+" (deleted)")
		}
	}
	return out
}

// modelTextLog accumulates the text parts of every model response — the
// evidence the production transcript drops.
type modelTextLog struct {
	mu    sync.Mutex
	texts []string
}

func (l *modelTextLog) add(s string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.texts = append(l.texts, s)
}

func (l *modelTextLog) drain() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.texts
	l.texts = nil
	return out
}

// teeLLM wraps the real LLM and records every non-partial text part the model
// emits, tool-call turns and final turns alike.
type teeLLM struct {
	adkmodel.LLM
	log *modelTextLog
}

func (l teeLLM) GenerateContent(ctx context.Context, req *adkmodel.LLMRequest, stream bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	inner := l.LLM.GenerateContent(ctx, req, stream)
	return func(yield func(*adkmodel.LLMResponse, error) bool) {
		for resp, err := range inner {
			if resp != nil && resp.Content != nil && !resp.Partial {
				for _, part := range resp.Content.Parts {
					if part != nil && strings.TrimSpace(part.Text) != "" {
						l.log.add(part.Text)
					}
				}
			}
			if !yield(resp, err) {
				return
			}
		}
	}
}
