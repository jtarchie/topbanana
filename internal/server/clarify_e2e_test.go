package server_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jtarchie/topbanana/internal/agent"
	"github.com/jtarchie/topbanana/internal/build"
	"github.com/jtarchie/topbanana/internal/events"
	"github.com/jtarchie/topbanana/internal/snapshot"
	"github.com/jtarchie/topbanana/internal/store"
)

// The ask_user tool (internal/agent.invokeAskUser) is never exercised across
// the HTTP/SSE boundary by the agent tests — they call invokeAskUser directly.
// questionRunner is a deterministic build.Runner that reproduces exactly what a
// real run does when the agent asks a question: it emits a TypeQuestion/PhaseAsk
// event via tracker.Ask and blocks on the returned channel until POST /clarify
// resolves it, then writes a valid page so the build completes through lint.
// This lets the tests below drive the full ask → SSE → card → /clarify → unblock
// loop with no live LLM.
const (
	clarifyQuestionID     = "q-clarify-test"
	clarifyQuestion       = "Should the landing page lead with a headline or a hero image?"
	clarifyRecommendation = "Lead with a bold headline"
	clarifyWhy            = "Text renders instantly and reads well on mobile."
)

//nolint:gochecknoglobals // test fixture shared by the HTTP + browser clarify tests.
var clarifyOptions = []string{"Bold headline", "Full-bleed hero image"}

type questionRunner struct {
	title, desc string
	// answered receives the answer the agent goroutine got back, so a test can
	// assert the value made the full round trip to the agent (not just that the
	// SSE stream echoed it). Buffered 1; nil disables recording.
	answered chan string
	// asked gates the question to the first Run only. A build invokes the runner
	// more than once (initial author pass, then a best-effort polish pass); we
	// ask on the first and let later passes just rewrite the page, so an
	// unanswered second question can't stall the build.
	asked atomic.Bool
}

func (r *questionRunner) Run(ctx context.Context, s *store.Store, req build.RunRequest, emit func(events.Event), tracker *events.Tracker) (agent.Usage, error) {
	if r.asked.CompareAndSwap(false, true) {
		// Mirror agent.invokeAskUser: register the question and block on the
		// answer channel with a timeout + cancellation escape hatch.
		ch := tracker.Ask(req.Slug, events.Event{
			Type:           events.TypeQuestion,
			Phase:          events.PhaseAsk,
			QuestionID:     clarifyQuestionID,
			Question:       clarifyQuestion,
			Recommendation: clarifyRecommendation,
			Why:            clarifyWhy,
			Options:        clarifyOptions,
		})

		var answer string
		select {
		case a, ok := <-ch:
			if ok {
				answer = a
			}
		case <-ctx.Done():
			return agent.Usage{}, fmt.Errorf("question runner cancelled: %w", ctx.Err())
		case <-time.After(20 * time.Second):
			return agent.Usage{}, errors.New("question runner: no answer within 20s")
		}
		if r.answered != nil {
			select {
			case r.answered <- answer:
			default:
			}
		}
	}

	// Write a valid, DaisyUI-linked page so the post-Run lint pass passes and
	// the build reaches terminal `completed`.
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseStart, Path: "/index.html"})
	err := s.Write(ctx, req.Slug, "index.html", stubIndexHTML, "text/html; charset=utf-8", nil)
	if err != nil {
		emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseError, Path: "/index.html", Message: err.Error()})

		return agent.Usage{}, fmt.Errorf("question runner write: %w", err)
	}
	emit(events.Event{Type: events.TypeTool, Tool: "write_file", Phase: events.PhaseDone, Path: "/index.html"})

	return agent.Usage{}, nil
}

func (r *questionRunner) Describe(_ context.Context, _ *store.Store, _ string, _ string) (agent.SiteDescription, error) {
	return agent.SiteDescription{Title: r.title, Description: r.desc}, nil
}

// TestClarify_EndToEnd drives the whole ask_user loop through the server: kick a
// build whose (stub) agent asks a question, consume /events/:slug until the
// PhaseAsk event, answer it via POST /clarify, then assert the PhaseAnswer event
// carries the answer, the agent goroutine received it, and the build completes.
//
//nolint:gocognit,cyclop // one end-to-end script that intentionally walks the loop.
func TestClarify_EndToEnd(t *testing.T) {
	st := minioStore(t)
	ctx := context.Background()
	snapSvc := snapshot.New(st, 0)
	runner := &questionRunner{title: "Clarify", desc: "clarify test", answered: make(chan string, 1)}
	handler := buildServerWithRunner(t, st, snapSvc, runner)
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	slug := "clfy-" + freshSlug(t)
	cleanupSlug(t, ctx, st, snapSvc, slug)

	// The answer we send — deliberately not one of the presented options, to
	// prove free-text answers flow through untouched.
	const answer = "Lead with a looping background video"

	postBuildForm(t, httpSrv.URL, url.Values{
		"template": {"blank"},
		"slug":     {slug},
		"prompt":   {"hello"},
	})

	// Open the SSE stream and read to a terminal status. When the PhaseAsk event
	// arrives we answer it inline (the POST returns once tracker.Resolve has
	// queued the PhaseAnswer event, so resuming the scan sees it before the
	// later `completed`).
	req, err := http.NewRequest(http.MethodGet, httpSrv.URL+"/events/"+slug, nil)
	if err != nil {
		t.Fatalf("new GET events: %v", err)
	}
	req.Host = "localhost"
	req.Header.Set("Accept", "text/event-stream")
	req.AddCookie(testSessionCookie)

	resp, err := (&http.Client{Timeout: 45 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("GET events: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events status: %d", resp.StatusCode)
	}

	doClarify := func(qid, ans string) error {
		body, _ := json.Marshal(clarifyRequestBody{QuestionID: qid, Answer: ans})
		creq, cerr := http.NewRequest(http.MethodPost, httpSrv.URL+"/clarify/"+slug, bytes.NewReader(body))
		if cerr != nil {
			return fmt.Errorf("new clarify request: %w", cerr)
		}
		creq.Host = "localhost"
		creq.Header.Set("Content-Type", "application/json")
		creq.AddCookie(testSessionCookie)
		cresp, cerr := (&http.Client{Timeout: 10 * time.Second}).Do(creq)
		if cerr != nil {
			return fmt.Errorf("post clarify: %w", cerr)
		}
		defer func() { _ = cresp.Body.Close() }()
		if cresp.StatusCode != http.StatusOK {
			return fmt.Errorf("clarify status %d", cresp.StatusCode)
		}

		return nil
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var (
		asked      events.Event
		sawAsk     bool
		answerEcho string
		sawAnswer  bool
		terminal   string
		clarifyErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			var ev events.Event
			if json.Unmarshal([]byte(payload), &ev) != nil {
				continue
			}
			switch {
			case ev.Type == events.TypeQuestion && ev.Phase == events.PhaseAsk:
				if !sawAsk {
					sawAsk = true
					asked = ev
					clarifyErr = doClarify(ev.QuestionID, answer)
				}
			case ev.Type == events.TypeQuestion && ev.Phase == events.PhaseAnswer:
				sawAnswer = true
				answerEcho = ev.Answer
			case ev.Type == events.TypeStatus && (ev.Status == "completed" || ev.Status == "failed"):
				terminal = ev.Status

				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(40 * time.Second):
		t.Fatalf("clarify loop did not complete in time (sawAsk=%v sawAnswer=%v terminal=%q)", sawAsk, sawAnswer, terminal)
	}

	if !sawAsk {
		t.Fatal("never received a question (PhaseAsk) event on the SSE stream")
	}
	if clarifyErr != nil {
		t.Fatalf("POST /clarify failed: %v", clarifyErr)
	}
	if asked.Question == "" || asked.Recommendation == "" || asked.Why == "" || len(asked.Options) == 0 {
		t.Errorf("question event missing fields: %+v", asked)
	}
	if !sawAnswer {
		t.Error("never received a PhaseAnswer event echoing the answer")
	}
	if answerEcho != answer {
		t.Errorf("PhaseAnswer event answer = %q, want %q", answerEcho, answer)
	}
	if terminal != "completed" {
		t.Errorf("build terminal status = %q, want completed", terminal)
	}

	select {
	case got := <-runner.answered:
		if got != answer {
			t.Errorf("agent received answer %q, want %q", got, answer)
		}
	case <-time.After(5 * time.Second):
		t.Error("agent goroutine never received the answer via the tracker")
	}
}

// TestClarify_HandlerErrors covers the /clarify handler's validation branches,
// which no other test touches. The test session is a super admin, so slug
// ownership is bypassed and any valid slug reaches the handler.
func TestClarify_HandlerErrors(t *testing.T) {
	st := minioStore(t)
	snapSvc := snapshot.New(st, 0)
	handler := buildServerWithRunner(t, st, snapSvc, &stubRunner{title: "x", desc: "y"})
	httpSrv := httptest.NewServer(handler)
	t.Cleanup(httpSrv.Close)

	slug := "clfy-" + freshSlug(t)

	post := func(t *testing.T, body string) int {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, httpSrv.URL+"/clarify/"+slug, strings.NewReader(body))
		if err != nil {
			t.Fatalf("new POST /clarify: %v", err)
		}
		req.Host = "localhost"
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(testSessionCookie)
		resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("POST /clarify: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()

		return resp.StatusCode
	}

	t.Run("missing question_id", func(t *testing.T) {
		if got := post(t, `{"answer":"hello"}`); got != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", got)
		}
	})

	t.Run("unknown question_id", func(t *testing.T) {
		// Valid shape, but no build is in flight so nothing is pending: Resolve
		// returns false → 404.
		if got := post(t, `{"question_id":"no-such-question","answer":"hello"}`); got != http.StatusNotFound {
			t.Errorf("status = %d, want 404", got)
		}
	})

	t.Run("oversized answer", func(t *testing.T) {
		// Between the handler's 4 KiB field cap and the route's 32 KiB body cap,
		// so it clears the body-limit middleware and trips the handler's own
		// length check → 413.
		big := `{"question_id":"q","answer":"` + strings.Repeat("a", 8*1024) + `"}`
		if got := post(t, big); got != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want 413", got)
		}
	})
}

// clarifyRequestBody mirrors the server's unexported clarifyRequest so the test
// can marshal the same JSON shape the handler binds.
type clarifyRequestBody struct {
	QuestionID string `json:"question_id"`
	Answer     string `json:"answer"`
}
