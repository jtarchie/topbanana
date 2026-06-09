package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/labstack/echo/v5"

	"github.com/jtarchie/topbanana/internal/events"
)

// This file owns the build-progress surface: the /status/:slug JSON poll and
// the /events/:slug Server-Sent-Events stream the workspace page consumes
// mid-build, plus the SSE framing helpers. Extracted from server.go so the
// streaming/replay logic lives in one place.

func (s *Server) statusHandler(c *echo.Context) error {
	slug := c.Param("slug")
	status := s.events.Get(slug)
	if status == nil {
		status = &events.Status{Slug: slug, Status: "unknown"}
	}
	return c.JSON(http.StatusOK, status) //nolint:wrapcheck
}

// eventsHandler streams a slug's build events as SSE. It first replays any
// past events so a late connection still sees what happened, then forwards
// live events until the build hits a terminal status or the client
// disconnects. Each frame carries an `id:` set to the event's index in the
// slug's history; on reconnect EventSource sends Last-Event-ID back and we
// skip past frames the client already rendered — otherwise the page shows
// the entire history twice every time the SSE connection blips.
func (s *Server) eventsHandler(c *echo.Context) error {
	slug := c.Param("slug")
	w := c.Response()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flush := func() {
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	lastID := parseLastEventID(c.Request().Header.Get("Last-Event-ID"))

	history, sub, terminal := s.events.Subscribe(slug)
	if sub == nil {
		// No status known for this slug — emit a single "unknown" frame and bail.
		_ = writeSSE(w, events.Event{Type: events.TypeStatus, Status: "unknown", Time: time.Now()})
		flush()
		return nil
	}
	defer s.events.Unsubscribe(slug, sub)

	for i, e := range history {
		if i <= lastID {
			continue
		}
		err := writeSSEWithID(w, i, e)
		if err != nil {
			return nil //nolint:nilerr // client gone, just stop streaming
		}
	}
	flush()
	if terminal {
		return nil
	}

	// nextID picks up where history left off so live frames continue the
	// same monotonic sequence the client uses for Last-Event-ID.
	streamLiveEvents(c.Request().Context(), w, sub, len(history), flush)
	return nil
}

// parseLastEventID reads the SSE reconnect cursor from the request header.
// Returns -1 when absent or malformed, which makes the caller's "skip
// indices <= lastID" loop replay the entire history.
func parseLastEventID(h string) int {
	if h == "" {
		return -1
	}
	v, err := strconv.Atoi(h)
	if err != nil {
		return -1
	}
	return v
}

// streamLiveEvents pumps live events from sub to w, assigning each one a
// monotonically-increasing SSE id starting at startID so the sequence
// continues from the history that was already replayed. Returns when the
// channel closes, the request context is canceled, the write fails, or
// the build reaches a terminal status.
func streamLiveEvents(ctx context.Context, w io.Writer, sub chan events.Event, startID int, flush func()) {
	nextID := startID
	for {
		select {
		case e, ok := <-sub:
			if !ok {
				return
			}
			err := writeSSEWithID(w, nextID, e)
			if err != nil {
				return
			}
			nextID++
			flush()
			if e.Type == events.TypeStatus && events.IsTerminal(e.Status) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// writeSSE writes a frame without an event id. Reserved for one-shot
// sentinel frames like the "unknown" status that aren't part of the
// history sequence and shouldn't influence the client's Last-Event-ID.
func writeSSE(w io.Writer, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", payload)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}

// writeSSEWithID writes a frame stamped with `id: <n>` so EventSource
// records it and echoes the latest one back as Last-Event-ID on reconnect.
func writeSSEWithID(w io.Writer, id int, event events.Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	_, err = fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, payload)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	return nil
}
