package events

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTracker_StartCompleteFail(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("a")
	if got := tr.Get("a"); got == nil || got.Status != StatusBuilding {
		t.Fatalf("after Start, status = %+v, want building", got)
	}

	tr.Complete("a")
	got := tr.Get("a")
	if got == nil || got.Status != StatusCompleted {
		t.Fatalf("after Complete, status = %+v, want completed", got)
	}
	if got.Error != "" {
		t.Errorf("after Complete, Error = %q, want empty", got.Error)
	}

	tr.Start("b")
	tr.Fail("b", errors.New("boom"))
	got = tr.Get("b")
	if got == nil || got.Status != StatusFailed {
		t.Fatalf("after Fail, status = %+v, want failed", got)
	}
	if got.Error != "boom" {
		t.Errorf("after Fail, Error = %q, want %q", got.Error, "boom")
	}
}

func TestTracker_GetUnknownReturnsNil(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	if got := tr.Get("nope"); got != nil {
		t.Errorf("Get on unknown slug = %+v, want nil", got)
	}
}

func TestTracker_EmitFanoutToLiveSubscribers(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, ch, terminal := tr.Subscribe("s")
	if terminal {
		t.Fatal("subscribe on building slug returned terminal=true")
	}
	if ch == nil {
		t.Fatal("subscribe returned nil channel")
	}

	tr.Emit("s", Event{Type: TypeTool, Tool: "write_file", Phase: PhaseStart, Path: "/index.html"})

	select {
	case ev := <-ch:
		if ev.Type != TypeTool || ev.Tool != "write_file" || ev.Phase != PhaseStart {
			t.Errorf("got event %+v", ev)
		}
		if ev.Time.IsZero() {
			t.Errorf("Emit did not stamp Time")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber did not receive event")
	}
}

func TestTracker_EmitDropsForSlowSubscriber(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, ch, _ := tr.Subscribe("s")

	// Subscribe channel has a buffer of 64. Emit 200 events without reading;
	// the emit path must not block.
	done := make(chan struct{})
	go func() {
		for range 200 {
			tr.Emit("s", Event{Type: TypeTool, Tool: "write_file"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on slow subscriber")
	}

	// The channel buffer should hold up to 64 events; the rest are dropped.
	// Drain a few to prove the channel is still alive.
	drained := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				t.Fatal("channel closed unexpectedly")
			}
			drained++
			if drained >= 10 {
				return
			}
		case <-time.After(50 * time.Millisecond):
			if drained == 0 {
				t.Fatal("no events drained from subscriber")
			}
			return
		}
	}
}

func TestTracker_SubscribeReplaysHistory(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	tr.Emit("s", Event{Type: TypeTool, Tool: "write_file", Phase: PhaseStart, Path: "/a"})
	tr.Emit("s", Event{Type: TypeTool, Tool: "write_file", Phase: PhaseDone, Path: "/a"})

	history, ch, terminal := tr.Subscribe("s")
	if terminal {
		t.Errorf("Subscribe on building slug returned terminal=true")
	}
	if len(history) != 3 { // Start + 2 tool events
		t.Fatalf("history len = %d, want 3", len(history))
	}
	if history[0].Type != TypeStatus || history[0].Status != StatusBuilding {
		t.Errorf("history[0] = %+v, want building status", history[0])
	}
	if history[2].Path != "/a" || history[2].Phase != PhaseDone {
		t.Errorf("history[2] = %+v", history[2])
	}

	// New event after subscribe arrives on the channel.
	tr.Emit("s", Event{Type: TypeTool, Tool: "write_file", Phase: PhaseDone, Path: "/b"})
	select {
	case ev := <-ch:
		if ev.Path != "/b" {
			t.Errorf("got event %+v, want path /b", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("subscriber did not receive post-subscribe event")
	}
}

func TestTracker_SubscribeUnknownSlug(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	history, ch, terminal := tr.Subscribe("nope")
	if history != nil || ch != nil {
		t.Errorf("Subscribe on unknown slug returned history=%v ch=%v, want nil", history, ch)
	}
	if !terminal {
		t.Errorf("Subscribe on unknown slug returned terminal=false, want true")
	}
}

func TestTracker_SubscribeOnTerminalSlugSeesTerminalFlag(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	tr.Complete("s")

	history, ch, terminal := tr.Subscribe("s")
	if !terminal {
		t.Errorf("Subscribe after Complete returned terminal=false")
	}
	if ch == nil {
		t.Errorf("Subscribe should still return a channel for drain")
	}
	if len(history) != 2 {
		t.Errorf("history len = %d, want 2", len(history))
	}
}

func TestTracker_ForgetClosesSubscribers(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, ch, _ := tr.Subscribe("s")

	tr.Forget("s")

	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Forget did not close subscriber channel")
	}

	if got := tr.Get("s"); got != nil {
		t.Errorf("Get after Forget = %+v, want nil", got)
	}
}

func TestTracker_ForgetUnknownSlugIsNoop(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Forget("nope") // must not panic
}

func TestTracker_Unsubscribe(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, ch, _ := tr.Subscribe("s")

	tr.Unsubscribe("s", ch)

	// Channel should be closed.
	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after Unsubscribe, got value")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Unsubscribe did not close channel")
	}

	// Emitting more shouldn't panic (sending on closed channel would panic;
	// Unsubscribe must remove the channel from the subscriber set first).
	tr.Emit("s", Event{Type: TypeTool, Tool: "write_file"})
}

func TestTracker_UnsubscribeIdempotent(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, ch, _ := tr.Subscribe("s")
	tr.Unsubscribe("s", ch)
	// Second call should be a noop, not a panic.
	tr.Unsubscribe("s", ch)
	tr.Unsubscribe("missing", ch)
}

func TestTracker_SweepDropsTerminalAfterTTL(t *testing.T) {
	t.Parallel()

	tr := NewTracker()

	// Plant a completed entry with a Finished time far in the past so the
	// sweep evicts it.
	tr.Emit("done", Event{Type: TypeStatus, Status: StatusBuilding})
	old := time.Now().Add(-2 * TerminalTTL)
	tr.Emit("done", Event{Type: TypeStatus, Status: StatusCompleted, Time: old})

	// And a still-building entry that must survive.
	tr.Start("alive")

	// Drive sweep directly with a future-enough "now" so the completed
	// entry's Finished < cutoff.
	tr.sweep(time.Now().Add(TerminalTTL))

	if got := tr.Get("done"); got != nil {
		t.Errorf("sweep did not evict terminal entry: %+v", got)
	}
	if got := tr.Get("alive"); got == nil || got.Status != StatusBuilding {
		t.Errorf("sweep evicted live entry: %+v", got)
	}
}

func TestTracker_SweepClosesSubscribersOnEviction(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Emit("done", Event{Type: TypeStatus, Status: StatusBuilding})
	old := time.Now().Add(-2 * TerminalTTL)
	tr.Emit("done", Event{Type: TypeStatus, Status: StatusCompleted, Time: old})

	_, ch, _ := tr.Subscribe("done")

	tr.sweep(time.Now().Add(TerminalTTL))

	select {
	case _, ok := <-ch:
		if ok {
			t.Errorf("expected channel close after sweep eviction")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("sweep did not close subscriber channel on eviction")
	}
}

func TestTracker_SweepLeavesNonTerminalAlone(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	tr.sweep(time.Now().Add(10 * TerminalTTL))
	if got := tr.Get("s"); got == nil {
		t.Errorf("sweep evicted building slug")
	}
}

func TestTracker_EmitConcurrent(t *testing.T) {
	t.Parallel()

	// Run with -race to catch races in the Emit path. We don't assert on the
	// final count because the slow-subscriber drop policy means some events
	// won't be observed by the subscriber — we only assert there's no
	// concurrent-map-write panic and the run completes.
	tr := NewTracker()
	tr.Start("s")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				tr.Emit("s", Event{Type: TypeTool, Tool: "write_file"})
			}
		}()
	}
	wg.Wait()

	got := tr.Get("s")
	if got == nil {
		t.Fatal("slug missing after concurrent emits")
	}
}

func TestTracker_EmitOnUnknownSlugCreatesEntry(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	// Emit without prior Start — the tracker is supposed to lazily create
	// the status entry so events aren't lost. This matches the production
	// flow where the build orchestrator emits tool events before the
	// status=building event in some paths.
	tr.Emit("late", Event{Type: TypeTool, Tool: "write_file", Phase: PhaseStart})
	got := tr.Get("late")
	if got == nil {
		t.Fatal("Emit on unknown slug did not create entry")
	}
}

func TestTracker_AskEmitsEvent(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, subs, _ := tr.Subscribe("s")

	q := Event{
		Type:       TypeQuestion,
		Phase:      PhaseAsk,
		QuestionID: "q1",
		Question:   "Which colour?",
	}
	tr.Ask("s", q)

	select {
	case ev := <-subs:
		if ev.Type != TypeQuestion || ev.Phase != PhaseAsk || ev.QuestionID != "q1" {
			t.Errorf("unexpected event from Ask: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Ask did not emit event to subscriber")
	}
}

func TestTracker_ResolveDeliversAnswer(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")
	_, subs, _ := tr.Subscribe("s")

	ch := tr.Ask("s", Event{Type: TypeQuestion, Phase: PhaseAsk, QuestionID: "q1"})
	<-subs // drain the ask event

	if !tr.Resolve("s", "q1", "Green") {
		t.Fatal("Resolve returned false for known question")
	}

	select {
	case answer, open := <-ch:
		if !open {
			t.Fatal("channel was closed before answer arrived")
		}
		if answer != "Green" {
			t.Errorf("got answer %q, want %q", answer, "Green")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Ask channel did not receive answer")
	}

	select {
	case ev := <-subs:
		if ev.Type != TypeQuestion || ev.Phase != PhaseAnswer || ev.Answer != "Green" {
			t.Errorf("unexpected PhaseAnswer event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Resolve did not emit PhaseAnswer event")
	}
}

func TestTracker_ResolveStaleFalse(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")

	// Resolve on unknown question_id returns false.
	if tr.Resolve("s", "no-such-id", "x") {
		t.Error("Resolve on unknown question_id returned true")
	}
	// Resolve on unknown slug also returns false.
	if tr.Resolve("nope", "q1", "x") {
		t.Error("Resolve on unknown slug returned true")
	}
}

func TestTracker_TerminalStatusClosesPending(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")

	ch := tr.Ask("s", Event{
		Type:       TypeQuestion,
		Phase:      PhaseAsk,
		QuestionID: "q1",
		Question:   "Test?",
	})

	// Completing the build should close the pending channel.
	tr.Complete("s")

	select {
	case _, open := <-ch:
		if open {
			t.Error("pending channel should be closed on terminal status")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pending channel was not closed after Complete")
	}
}

func TestTracker_EmitTimeout(t *testing.T) {
	t.Parallel()

	tr := NewTracker()
	tr.Start("s")

	_, subs, _ := tr.Subscribe("s")

	ch := tr.Ask("s", Event{
		Type:       TypeQuestion,
		Phase:      PhaseAsk,
		QuestionID: "q2",
		Question:   "Timeout test?",
	})
	// drain the ask event
	<-subs

	tr.EmitTimeout("s", "q2", "default answer")

	// channel must be closed (agent treats closed channel as timeout)
	select {
	case _, open := <-ch:
		if open {
			t.Error("channel should be closed after EmitTimeout")
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("channel not closed after EmitTimeout")
	}

	// PhaseTimeout event should have been emitted.
	select {
	case ev := <-subs:
		if ev.Type != TypeQuestion || ev.Phase != PhaseTimeout || ev.Answer != "default answer" {
			t.Errorf("unexpected timeout event: %+v", ev)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("EmitTimeout did not emit PhaseTimeout event")
	}

	// A subsequent Resolve on the same question_id must return false.
	if tr.Resolve("s", "q2", "late answer") {
		t.Error("Resolve after EmitTimeout should return false")
	}
}
