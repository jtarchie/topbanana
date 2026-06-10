// Package events provides an in-process pub/sub tracker for build lifecycle
// events. Each slug owns one Status entry; emitters append events and live
// subscribers receive them on a buffered channel. Terminal entries are evicted
// by a background sweep after TerminalTTL.
package events

import (
	"sync"
	"time"
)

// Question event phases (used with TypeQuestion events).
const (
	PhaseAsk     = "ask"     // agent asked, waiting for user
	PhaseAnswer  = "answer"  // user replied
	PhaseTimeout = "timeout" // user didn't reply; recommendation auto-accepted
)

const (
	// TerminalTTL is how long a completed/failed Status stays in the tracker
	// after termination. The progress page polls every few seconds, so this
	// must be comfortably larger than that to guarantee the redirect is observed.
	TerminalTTL   = time.Minute
	sweepInterval = time.Minute
)

// Status string constants used by Event.Status and Status.Status.
const (
	StatusBuilding  = "building"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusLinting   = "linting"
	StatusPolishing = "polishing"
	StatusRetry     = "retry"
)

// IsTerminal reports whether a status means the run has finished (either
// outcome). Terminal entries stop the live SSE stream and become eligible for
// the TerminalTTL sweep.
func IsTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed
}

// IsActive reports whether a status means a run is still in flight. It is the
// complement of IsTerminal over the known statuses, exposed separately so
// callers that only care about "should I show the progress strip" don't have
// to enumerate the non-terminal states themselves.
func IsActive(status string) bool {
	switch status {
	case StatusBuilding, StatusLinting, StatusRetry, StatusPolishing:
		return true
	default:
		return false
	}
}

// Event types.
const (
	TypeStatus   = "status"
	TypeTool     = "tool"
	TypeFunction = "function"
	TypeQuestion = "question"
)

// Tool phases (also used for TypeFunction).
const (
	PhaseStart  = "start"
	PhaseDone   = "done"
	PhaseError  = "error"
	PhaseInvoke = "invoke"
	PhaseLog    = "log"
)

// Event is the payload streamed to subscribers and recorded for replay on
// reconnect.
type Event struct {
	Type    string    `json:"type"`              // one of the Type* consts (status | tool | function | question)
	Status  string    `json:"status,omitempty"`  // for type=status: one of the Status* consts
	Tool    string    `json:"tool,omitempty"`    // for type=tool/function: the tool that acted (see the build agent's tool set)
	Phase   string    `json:"phase,omitempty"`   // for type=tool/function: a Phase* const; for type=question: PhaseAsk|PhaseAnswer|PhaseTimeout
	Path    string    `json:"path,omitempty"`    // for type=tool: file path the tool acted on
	Message string    `json:"message,omitempty"` // optional human-readable detail (errors, retry reason)
	Detail  string    `json:"detail,omitempty"`  // for type=status/failed: raw technical text behind the friendly Message
	Time    time.Time `json:"time"`

	// Question fields — only set when Type == TypeQuestion.
	QuestionID     string   `json:"question_id,omitempty"`
	Question       string   `json:"question,omitempty"`
	Recommendation string   `json:"recommendation,omitempty"`
	Why            string   `json:"why,omitempty"`
	Options        []string `json:"options,omitempty"`
	Answer         string   `json:"answer,omitempty"` // set on PhaseAnswer / PhaseTimeout
}

// Status is the per-slug record of build state plus event history. Events and
// subs are guarded by the parent Tracker's mutex.
type Status struct {
	Slug     string    `json:"slug"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"-"`

	Events  []Event                 `json:"-"`
	subs    map[chan Event]struct{} `json:"-"`
	pending map[string]chan string  `json:"-"` // keyed by question_id; buffered 1
}

// Tracker holds the active build map. The zero value is not usable; call
// NewTracker which also spawns the sweep goroutine.
type Tracker struct {
	mu        sync.Mutex
	m         map[string]*Status
	done      chan struct{}
	closeOnce sync.Once
}

// NewTracker spawns a background sweep goroutine. Call Close to terminate
// it; production callers tie that to the server shutdown, tests defer it.
func NewTracker() *Tracker {
	t := &Tracker{m: make(map[string]*Status), done: make(chan struct{})}
	go t.sweepLoop()
	return t
}

// Close stops the sweep goroutine. Idempotent.
func (t *Tracker) Close() {
	t.closeOnce.Do(func() { close(t.done) })
}

func (t *Tracker) Start(slug string) {
	t.Emit(slug, Event{Type: TypeStatus, Status: StatusBuilding})
}

func (t *Tracker) Complete(slug string) {
	t.Emit(slug, Event{Type: TypeStatus, Status: StatusCompleted})
}

func (t *Tracker) Fail(slug string, err error) {
	t.Emit(slug, Event{Type: TypeStatus, Status: StatusFailed, Message: err.Error()})
}

// Emit records an event on the slug's status, fans it out to any live
// subscribers (dropping for slow consumers — they can use replay on reconnect),
// and updates Finished when the status reaches a final state.
func (t *Tracker) Emit(slug string, event Event) {
	if event.Time.IsZero() {
		event.Time = time.Now()
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	s, ok := t.m[slug]
	if !ok {
		s = &Status{Slug: slug, subs: map[chan Event]struct{}{}}
		t.m[slug] = s
	}
	s.Events = append(s.Events, event)
	if event.Type == TypeStatus {
		s.Status = event.Status
		switch event.Status {
		case StatusCompleted:
			s.Finished = event.Time
			s.Error = ""
			s.closePending()
		case StatusFailed:
			s.Finished = event.Time
			s.Error = event.Message
			s.closePending()
		}
	}
	for sub := range s.subs {
		select {
		case sub <- event:
		default:
		}
	}
}

// Get returns a copy of the public Status fields for the slug, or nil if
// unknown. Internal fields (Events, subs) are not copied.
func (t *Tracker) Get(slug string) *Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[slug]
	if !ok {
		return nil
	}
	return &Status{Slug: s.Slug, Status: s.Status, Error: s.Error, Finished: s.Finished}
}

// Subscribe returns a snapshot of past events plus a channel that receives new
// ones. terminal indicates the build already finished — callers should still
// drain the channel for any concurrent emits but can exit promptly.
func (t *Tracker) Subscribe(slug string) (history []Event, ch chan Event, terminal bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[slug]
	if !ok {
		return nil, nil, true
	}
	if s.subs == nil {
		s.subs = map[chan Event]struct{}{}
	}
	ch = make(chan Event, 64)
	s.subs[ch] = struct{}{}
	history = append([]Event(nil), s.Events...)
	terminal = !s.Finished.IsZero()
	return history, ch, terminal
}

// Forget drops a slug's Status and closes any live subscriber channels. Used
// when an app is deleted so /status/:slug stops reporting on a ghost and
// goroutines waiting on Subscribe can exit.
func (t *Tracker) Forget(slug string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[slug]
	if !ok {
		return
	}
	for ch := range s.subs {
		close(ch)
	}
	delete(t.m, slug)
}

// closePending closes and removes all pending question channels. Must be called
// with t.mu held. Prevents goroutine leaks when a build reaches a terminal
// state before the user answers a question.
func (s *Status) closePending() {
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
}

// Ask stores a buffered channel for the given question event, emits the event,
// and returns the channel. The agent goroutine reads from it (with a timeout).
// q.QuestionID must be non-empty and unique within the slug.
func (t *Tracker) Ask(slug string, q Event) <-chan string {
	ch := make(chan string, 1)
	t.mu.Lock()
	s, ok := t.m[slug]
	if !ok {
		s = &Status{Slug: slug, subs: map[chan Event]struct{}{}}
		t.m[slug] = s
	}
	if s.pending == nil {
		s.pending = map[string]chan string{}
	}
	s.pending[q.QuestionID] = ch
	t.mu.Unlock()

	t.Emit(slug, q)
	return ch
}

// Resolve delivers an answer to the pending question identified by questionID.
// It emits a PhaseAnswer event and returns true. Returns false if no pending
// question with that ID exists (stale or already answered).
func (t *Tracker) Resolve(slug, questionID, answer string) bool {
	t.mu.Lock()
	s, ok := t.m[slug]
	if !ok {
		t.mu.Unlock()
		return false
	}
	ch, exists := s.pending[questionID]
	if !exists {
		t.mu.Unlock()
		return false
	}
	delete(s.pending, questionID)
	t.mu.Unlock()

	ch <- answer
	close(ch)

	t.Emit(slug, Event{
		Type:       TypeQuestion,
		Phase:      PhaseAnswer,
		QuestionID: questionID,
		Answer:     answer,
	})
	return true
}

// EmitTimeout removes the pending question channel (preventing a stale Resolve)
// and emits a PhaseTimeout event so the workspace can remove the question card.
// It is called by the agent when the user-response timer fires.
func (t *Tracker) EmitTimeout(slug, questionID, recommendation string) {
	t.mu.Lock()
	if s, ok := t.m[slug]; ok {
		if ch, exists := s.pending[questionID]; exists {
			delete(s.pending, questionID)
			close(ch)
		}
	}
	t.mu.Unlock()

	t.Emit(slug, Event{
		Type:       TypeQuestion,
		Phase:      PhaseTimeout,
		QuestionID: questionID,
		Answer:     recommendation,
	})
}

func (t *Tracker) Unsubscribe(slug string, ch chan Event) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.m[slug]
	if !ok {
		return
	}
	if _, alive := s.subs[ch]; !alive {
		return
	}
	delete(s.subs, ch)
	close(ch)
}

func (t *Tracker) sweepLoop() {
	tick := time.NewTicker(sweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-t.done:
			return
		case now := <-tick.C:
			t.sweep(now)
		}
	}
}

// sweep removes terminal entries older than TerminalTTL. "building" entries
// are never swept — a hung agent is a separate problem, surfaced as a stuck
// progress page rather than silently disappearing state. Subscribers attached
// to evicted entries have their channels closed so their goroutines can exit.
func (t *Tracker) sweep(now time.Time) {
	cutoff := now.Add(-TerminalTTL)
	t.mu.Lock()
	defer t.mu.Unlock()
	for slug, s := range t.m {
		if !s.Finished.IsZero() && s.Finished.Before(cutoff) {
			for ch := range s.subs {
				close(ch)
			}
			delete(t.m, slug)
		}
	}
}
