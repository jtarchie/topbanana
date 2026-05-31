package agent

import (
	"testing"

	"google.golang.org/genai"
)

func TestUsageAddSumsAcrossResponses(t *testing.T) {
	t.Parallel()

	var u Usage
	u = u.add(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        100,
		CachedContentTokenCount: 40,
		CandidatesTokenCount:    20,
		ThoughtsTokenCount:      5,
		ToolUsePromptTokenCount: 8,
		TotalTokenCount:         133,
	})
	u = u.add(&genai.GenerateContentResponseUsageMetadata{
		PromptTokenCount:        50,
		CachedContentTokenCount: 50,
		CandidatesTokenCount:    10,
		TotalTokenCount:         60,
	})

	if u.Prompt != 150 || u.Cached != 90 || u.Candidates != 30 || u.Thoughts != 5 || u.ToolUse != 8 || u.Total != 193 {
		t.Fatalf("unexpected totals: %+v", u)
	}
	if u.Responses != 2 {
		t.Errorf("Responses = %d, want 2", u.Responses)
	}
}

func TestUsageAddNilIsNoOp(t *testing.T) {
	t.Parallel()

	u := Usage{Prompt: 10, Responses: 1}
	got := u.add(nil)
	if got != u {
		t.Errorf("add(nil) mutated usage: got %+v want %+v", got, u)
	}
}

func TestUsageCacheHitRatio(t *testing.T) {
	t.Parallel()

	// Empty run never divides by zero.
	if r := (Usage{}).CacheHitRatio(); r != 0 {
		t.Errorf("empty CacheHitRatio = %v, want 0", r)
	}
	if r := (Usage{Prompt: 200, Cached: 150}).CacheHitRatio(); r != 0.75 {
		t.Errorf("CacheHitRatio = %v, want 0.75", r)
	}
}
