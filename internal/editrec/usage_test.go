package editrec

import "testing"

func TestRecorderAddUsageAccumulates(t *testing.T) {
	t.Parallel()

	r := New("slug", "build", "prompt", "", 0)
	// Two runs (author + one lint-fix retry) fold into a single transcript
	// total — the figure should be the whole cost of producing the site.
	r.AddUsage(Usage{Prompt: 100, Cached: 40, Candidates: 20, Thoughts: 5, ToolUse: 8, Total: 133, Responses: 2})
	r.AddUsage(Usage{Prompt: 50, Cached: 10, Candidates: 5, Total: 55, Responses: 1})

	got := r.transcript.Usage
	want := Usage{Prompt: 150, Cached: 50, Candidates: 25, Thoughts: 5, ToolUse: 8, Total: 188, Responses: 3}
	if got != want {
		t.Errorf("accumulated usage = %+v, want %+v", got, want)
	}
}

func TestRecorderAddUsageNilReceiver(t *testing.T) {
	t.Parallel()

	// recordUsage feeds a nil recorder when transcript capture is disabled;
	// it must be a no-op rather than panic.
	var r *Recorder
	r.AddUsage(Usage{Prompt: 10})
}
