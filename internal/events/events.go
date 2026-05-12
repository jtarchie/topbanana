// Package events provides an in-process pub/sub tracker for build lifecycle
// events. Each slug owns one Status entry; emitters append events and live
// subscribers receive them on a buffered channel. Terminal entries are evicted
// by a background sweep after TerminalTTL.
package events

import (
	"sync"
	"time"
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
	StatusRetry     = "retry"
)

// Event types.
const (
	TypeStatus = "status"
	TypeTool   = "tool"
)

// Tool phases.
const (
	PhaseStart = "start"
	PhaseDone  = "done"
	PhaseError = "error"
)

// Event is the payload streamed to subscribers and recorded for replay on
// reconnect.
type Event struct {
	Type    string    `json:"type"`              // "status" | "tool"
	Status  string    `json:"status,omitempty"`  // for type=status: building|completed|failed|linting|retry
	Tool    string    `json:"tool,omitempty"`    // for type=tool: write_file|read_file|list_files|list_assets
	Phase   string    `json:"phase,omitempty"`   // for type=tool: start|done|error
	Path    string    `json:"path,omitempty"`    // for type=tool: file path the tool acted on
	Message string    `json:"message,omitempty"` // optional human-readable detail (errors, retry reason)
	Time    time.Time `json:"time"`
}

// Status is the per-slug record of build state plus event history. Events and
// subs are guarded by the parent Tracker's mutex.
type Status struct {
	Slug     string    `json:"slug"`
	Status   string    `json:"status"`
	Error    string    `json:"error,omitempty"`
	Finished time.Time `json:"-"`

	Events []Event                 `json:"-"`
	subs   map[chan Event]struct{} `json:"-"`
}

// Tracker holds the active build map. The zero value is not usable; call
// NewTracker which also spawns the sweep goroutine.
type Tracker struct {
	mu sync.Mutex
	m  map[string]*Status
}

// NewTracker spawns a background sweep goroutine that lives for the lifetime
// of the process; we don't bother with shutdown coordination because the only
// consumer is the long-running HTTP server.
func NewTracker() *Tracker {
	t := &Tracker{m: make(map[string]*Status)}
	go t.sweepLoop()
	return t
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
		case StatusFailed:
			s.Finished = event.Time
			s.Error = event.Message
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
	for now := range tick.C {
		t.sweep(now)
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
